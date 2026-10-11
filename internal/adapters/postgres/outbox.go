package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/UmBender/backend-challenge-go/internal/domain/event"
)

type outboxRepo struct{ q pgx.Tx }

// Append runs inside the business transaction, so an event exists if and only
// if its change was committed (§5).
func (r outboxRepo) Append(ctx context.Context, events ...event.Envelope) error {
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", e.Type(), err)
		}
		// Due immediately.
		if _, err := r.q.Exec(ctx, `INSERT INTO outbox_events
			(event_id, aggregate_type, aggregate_id, event_type, event_version, payload, occurred_at, next_attempt_at)
			VALUES ($1, 'Wallet', $2, $3, $4, $5, $6, $6)`,
			e.ID(), e.AggregateID(), string(e.Type()), e.Version(), payload, e.OccurredAt()); err != nil {
			return err
		}
	}
	return nil
}
