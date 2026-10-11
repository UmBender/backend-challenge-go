package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

// PendingWorker resumes operations waiting for their reference (REFUND before
// its BET, for example). The state lives in the row, so any replica resumes
// work started by another, including one that crashed.
type PendingWorker struct {
	tx     TxManager
	clock  Clock
	ids    IDs
	policy PendingPolicy
	log    *slog.Logger
	poll   time.Duration
}

func NewPendingWorker(tx TxManager, clock Clock, ids IDs, policy PendingPolicy, log *slog.Logger) *PendingWorker {
	return &PendingWorker{tx: tx, clock: clock, ids: ids, policy: policy, log: log.With("component", "pending-worker"), poll: time.Second}
}

// RunOnce resolves at most one due row and reports whether it found one.
// Each row is decided in its own transaction, with the same lock order as
// SubmitWager after its insert: transaction row, then wallet row.
func (u *PendingWorker) RunOnce(ctx context.Context) (bool, error) {
	now := u.clock.Now()
	var claimed *wager.Transaction
	err := u.tx.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		tx, err := r.Wagers().ClaimDuePending(ctx, now)
		if err != nil || tx == nil {
			return err
		}
		claimed = tx
		w, err := r.Wallets().GetForUpdate(ctx, tx.WalletID())
		if err != nil {
			return err
		}
		ref, err := loadReference(ctx, r, tx)
		if err != nil {
			return err
		}
		version := w.Version()
		d, err := tx.Apply(wager.ApplyInput{
			Wallet: w, Reference: ref, EntryID: u.ids.New(), Now: now,
			Retry: wager.RetryPolicy{
				NextAttemptAt: now.Add(Backoff(tx.Attempts()+1, u.policy.BackoffBase, u.policy.BackoffCap)),
				TTL:           u.policy.TTL,
				MaxAttempts:   u.policy.MaxAttempts,
			},
		})
		if err != nil {
			return err
		}
		return persistDecision(ctx, r, u.ids, tx, w, version, d, tx.IdempotencyKey(), "pending-worker", now)
	})
	if err == nil || claimed == nil || transient(ctx, err) {
		return claimed != nil, err
	}
	// A permanent failure would otherwise be retried forever: record it as
	// FAILED (PROCESSING_FAILED) for audit, in a separate transaction.
	u.log.ErrorContext(ctx, "pending operation failed permanently", "transactionId", claimed.ID(), "err", err)
	return true, u.tx.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		tx, err := r.Wagers().GetForUpdate(ctx, claimed.ID())
		if err != nil {
			return err
		}
		if tx.Status() != wager.StatusPendingReference {
			return nil
		}
		if err := tx.Fail(u.clock.Now()); err != nil {
			return err
		}
		return r.Wagers().Update(ctx, tx)
	})
}

func transient(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrConcurrentConflict) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// Run resolves due rows until none is left, then polls.
func (u *PendingWorker) Run(ctx context.Context) {
	for {
		found, err := u.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			u.log.WarnContext(ctx, "pending worker iteration failed", "err", err)
		}
		if found && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.poll):
		}
	}
}
