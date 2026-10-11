package postgres

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UmBender/backend-challenge-go/internal/app"
)

// OutboxStore implements app.OutboxStore. Each method is one autocommit
// statement, so no lock is held while the event travels to the broker.
type OutboxStore struct{ pool *pgxpool.Pool }

func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore { return &OutboxStore{pool: pool} }

// Claim selects due rows with FOR UPDATE SKIP LOCKED, so concurrent
// publishers take disjoint batches, and leases them in the same statement.
func (s *OutboxStore) Claim(ctx context.Context, owner string, limit int, lease time.Duration) ([]app.PendingEvent, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE outbox_events o
		   SET locked_by = $1, locked_until = now() + $3 * interval '1 millisecond'
		  FROM (SELECT event_id FROM outbox_events
		         WHERE published_at IS NULL AND next_attempt_at <= now()
		           AND (locked_until IS NULL OR locked_until < now())
		         ORDER BY occurred_at, event_id
		         LIMIT $2
		           FOR UPDATE SKIP LOCKED) due
		 WHERE o.event_id = due.event_id
		RETURNING o.event_id, o.aggregate_id, o.payload::text, o.attempts, o.occurred_at`,
		owner, limit, lease.Milliseconds())
	if err != nil {
		return nil, classify(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (app.PendingEvent, error) {
		var e app.PendingEvent
		var payload string
		err := row.Scan(&e.ID, &e.AggregateID, &payload, &e.Attempts, &e.OccurredAt)
		e.Payload = []byte(payload)
		return e, err
	})
	if err != nil {
		return nil, classify(err)
	}
	// RETURNING order is unspecified; send oldest first.
	slices.SortFunc(out, func(a, b app.PendingEvent) int { return a.OccurredAt.Compare(b.OccurredAt) })
	return out, nil
}

func (s *OutboxStore) MarkPublished(ctx context.Context, ids []uuid.UUID, owner string) error {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = id.String()
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		   SET published_at = now(), locked_by = NULL, locked_until = NULL, last_error = NULL
		 WHERE event_id = ANY($1::uuid[]) AND locked_by = $2 AND published_at IS NULL`, keys, owner)
	return classify(err)
}

func (s *OutboxStore) MarkFailed(ctx context.Context, id uuid.UUID, owner string, next time.Time, reason string) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		   SET attempts = attempts + 1, next_attempt_at = $3, last_error = $4,
		       locked_by = NULL, locked_until = NULL
		 WHERE event_id = $1 AND locked_by = $2 AND published_at IS NULL`, id, owner, next, reason)
	return classify(err)
}

func (s *OutboxStore) OldestUnpublished(ctx context.Context) (time.Time, bool, error) {
	var oldest time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT occurred_at FROM outbox_events
		 WHERE published_at IS NULL ORDER BY occurred_at LIMIT 1`).Scan(&oldest)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, classify(err)
	}
	return oldest, true, nil
}
