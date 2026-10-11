package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/UmBender/backend-challenge-go/internal/app"
)

type inboxRepo struct{ q pgx.Tx }

func (r inboxRepo) Claim(ctx context.Context, consumer, messageID string, hash []byte, now time.Time) (*app.InboxEntry, error) {
	tag, err := r.q.Exec(ctx, `INSERT INTO inbox_messages (consumer_name, message_id, payload_hash, received_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, consumer, messageID, hash, now)
	if err != nil || tag.RowsAffected() == 1 {
		return nil, err
	}
	var e app.InboxEntry
	var txID *uuid.UUID
	err = r.q.QueryRow(ctx, `SELECT payload_hash, transaction_id FROM inbox_messages
		WHERE consumer_name = $1 AND message_id = $2`, consumer, messageID).Scan(&e.PayloadHash, &txID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The conflicting delivery rolled back meanwhile: start over.
		return nil, app.ErrConcurrentConflict
	}
	if err != nil {
		return nil, err
	}
	if txID == nil {
		return nil, app.ErrInvariantViolation // completion shares the transaction of the claim
	}
	e.TransactionID = *txID
	return &e, nil
}

func (r inboxRepo) Complete(ctx context.Context, consumer, messageID string, transactionID uuid.UUID, now time.Time) error {
	_, err := r.q.Exec(ctx, `UPDATE inbox_messages SET transaction_id = $3, completed_at = $4
		WHERE consumer_name = $1 AND message_id = $2`, consumer, messageID, transactionID, now)
	return err
}
