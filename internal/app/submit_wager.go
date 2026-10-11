package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

// SubmitCommand is one external operation; HTTP and SQS build the same
// Request, so they share validation, hash and outcome.
type SubmitCommand struct {
	Request        wager.Request
	IdempotencyKey string
	// Not part of the payload hash.
	CorrelationID string
	CausationID   string
	// Inbox is set by the SQS consumer: the message is recorded in the same
	// transaction as its effects, and a redelivery replays the stored result.
	Inbox *InboxMessage
}

type InboxMessage struct {
	Consumer  string
	MessageID string
}

// SubmitResult carries the stored outcome and whether the call was a replay.
type SubmitResult struct {
	Transaction *wager.Transaction
	Replay      bool
}

type PendingPolicy struct {
	TTL         time.Duration
	MaxAttempts int
	BackoffBase time.Duration
	BackoffCap  time.Duration
}

// SubmitWager is the single use case behind HTTP, SQS and the pending worker.
type SubmitWager struct {
	tx     TxManager
	clock  Clock
	ids    IDs
	policy PendingPolicy
}

func NewSubmitWager(tx TxManager, clock Clock, ids IDs, policy PendingPolicy) *SubmitWager {
	return &SubmitWager{tx: tx, clock: clock, ids: ids, policy: policy}
}

// Under the wallet lock conflicts should not happen; this bounds retries of
// the races the indexes catch, in this one layer only.
const conflictRetries = 3

// Execute runs the whole operation in one SQL transaction:
//
//  1. INSERT as PENDING, ON CONFLICT DO NOTHING; a duplicate waits for the
//     first to commit, then is answered from the stored row.
//  2. Lock the wallet row (FOR NO KEY UPDATE): one wallet serializes here,
//     different wallets never meet.
//  3. Load the reference after the lock, so it reflects committed reversals.
//  4. Let the domain decide, persist everything, COMMIT.
//
// The lock order is always index entry, then one wallet row, so the flow
// cannot deadlock. Nothing is committed as PENDING on this path.
func (u *SubmitWager) Execute(ctx context.Context, cmd SubmitCommand) (SubmitResult, error) {
	// An invalid request is a 400 and never reaches the database.
	if err := cmd.Request.Validate(); err != nil {
		return SubmitResult{}, err
	}
	if err := wager.ValidateIdempotencyKey(cmd.IdempotencyKey); err != nil {
		return SubmitResult{}, err
	}
	hash, err := PayloadHash(cmd.Request)
	if err != nil {
		return SubmitResult{}, err
	}
	if cmd.CorrelationID == "" {
		cmd.CorrelationID = cmd.IdempotencyKey
	}
	// The inbox hash covers the business payload and the key, so a messageId
	// reused for anything else is detected.
	inboxHash := sha256.Sum256(append(bytes.Clone(hash), cmd.IdempotencyKey...))
	for attempt := 1; ; attempt++ {
		res, err := u.once(ctx, cmd, hash, inboxHash[:])
		if err == nil || !errors.Is(err, ErrConcurrentConflict) || attempt == conflictRetries {
			return res, err
		}
		// Retry the whole transaction, never a statement of an aborted one.
		select {
		case <-time.After(Backoff(attempt, 10*time.Millisecond, 100*time.Millisecond)):
		case <-ctx.Done():
			return SubmitResult{}, ctx.Err()
		}
	}
}

func (u *SubmitWager) once(ctx context.Context, cmd SubmitCommand, hash, inboxHash []byte) (SubmitResult, error) {
	now := u.clock.Now()
	tx, err := wager.NewExternal(u.ids.New(), cmd.Request, cmd.IdempotencyKey, hash, now)
	if err != nil {
		return SubmitResult{}, err
	}
	var res SubmitResult
	err = u.tx.WithinTx(ctx, func(ctx context.Context, r Repos) error {
		if cmd.Inbox != nil {
			seen, err := r.Inbox().Claim(ctx, cmd.Inbox.Consumer, cmd.Inbox.MessageID, inboxHash, now)
			if err != nil {
				return err
			}
			if seen != nil {
				if !bytes.Equal(seen.PayloadHash, inboxHash) {
					return ErrMessageReused
				}
				// Redelivery of a completed message: replay, no new effect.
				stored, err := r.Wagers().Get(ctx, seen.TransactionID)
				res = SubmitResult{Transaction: stored, Replay: true}
				return err
			}
		}
		inserted, err := r.Wagers().InsertIfAbsent(ctx, tx)
		if err != nil {
			return err
		}
		if !inserted {
			if res, err = existing(ctx, r, cmd, hash); err != nil {
				return err
			}
			return completeInbox(ctx, r, cmd, res.Transaction, now)
		}
		w, err := r.Wallets().GetForUpdate(ctx, cmd.Request.WalletID)
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
				NextAttemptAt: now.Add(Backoff(1, u.policy.BackoffBase, u.policy.BackoffCap)),
				TTL:           u.policy.TTL,
				MaxAttempts:   u.policy.MaxAttempts,
			},
		})
		if err != nil {
			return err
		}
		if err := persistDecision(ctx, r, u.ids, tx, w, version, d, cmd.CorrelationID, cmd.CausationID, now); err != nil {
			return err
		}
		res = SubmitResult{Transaction: tx}
		return completeInbox(ctx, r, cmd, tx, now)
	})
	return res, err
}

func completeInbox(ctx context.Context, r Repos, cmd SubmitCommand, tx *wager.Transaction, now time.Time) error {
	if cmd.Inbox == nil {
		return nil
	}
	return r.Inbox().Complete(ctx, cmd.Inbox.Consumer, cmd.Inbox.MessageID, tx.ID(), now)
}

// existing looks up the key first: it identifies the request. Only a new
// key can reveal, by external id, a second key for the same operation.
func existing(ctx context.Context, r Repos, cmd SubmitCommand, hash []byte) (SubmitResult, error) {
	req := cmd.Request
	byKey, err := r.Wagers().FindByKey(ctx, req.ProviderID, cmd.IdempotencyKey)
	if err != nil {
		return SubmitResult{}, err
	}
	if byKey != nil {
		if !bytes.Equal(byKey.PayloadHash(), hash) {
			return SubmitResult{}, ErrIdempotencyKeyReused
		}
		// Replay: the stored outcome, never re-applied (§6.3).
		return SubmitResult{Transaction: byKey, Replay: true}, nil
	}
	byExt, err := r.Wagers().FindByExternalID(ctx, req.ProviderID, req.ExternalTransactionID)
	if err != nil {
		return SubmitResult{}, err
	}
	if byExt != nil {
		return SubmitResult{}, ErrExternalTransactionConflict
	}
	// The conflicting row was rolled back meanwhile: start over.
	return SubmitResult{}, ErrConcurrentConflict
}

func loadReference(ctx context.Context, r Repos, tx *wager.Transaction) (wager.Reference, error) {
	refExt := tx.ReferenceExternalID()
	if refExt == "" {
		return wager.Reference{}, nil
	}
	found, err := r.Wagers().FindByExternalID(ctx, tx.ProviderID(), refExt)
	if err != nil || found == nil {
		return wager.Reference{}, err
	}
	reversed, err := r.Wagers().HasProcessedReversal(ctx, found.ID())
	if err != nil {
		return wager.Reference{}, err
	}
	return wager.Reference{Tx: found, AlreadyReversed: reversed}, nil
}

// The balance update carries the version read under the lock; the ledger
// entry carries the new one, checked by UNIQUE (wallet_id, wallet_version).
func persistDecision(ctx context.Context, r Repos, ids IDs, tx *wager.Transaction, w *wallet.Wallet,
	versionBefore int64, d wager.Decision, correlationID, causationID string, now time.Time) error {
	if d.Entry != nil {
		if err := r.Wallets().UpdateBalance(ctx, w, versionBefore); err != nil {
			return err
		}
		if err := r.Ledger().Append(ctx, *d.Entry); err != nil {
			return err
		}
	}
	if err := r.Wagers().Update(ctx, tx); err != nil {
		return err
	}
	evs, err := envelopes(ids, correlationID, causationID, now, d.Events)
	if err != nil {
		return err
	}
	return r.Outbox().Append(ctx, evs...)
}
