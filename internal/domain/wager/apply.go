package wager

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/event"
	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

// Reference is loaded by the application under the wallet lock; the domain
// does no I/O.
type Reference struct {
	// nil if it does not exist (yet).
	Tx *Transaction
	// A PROCESSED REFUND or ROLLBACK already references Tx.
	AlreadyReversed bool
}

// The application computes NextAttemptAt (backoff with jitter needs
// randomness); the domain applies TTL and MaxAttempts.
type RetryPolicy struct {
	NextAttemptAt time.Time
	TTL           time.Duration
	MaxAttempts   int
}

// Identifiers and time are injected so decisions are deterministic.
type ApplyInput struct {
	Wallet    *wallet.Wallet
	Reference Reference
	EntryID   uuid.UUID
	Retry     RetryPolicy
	Now       time.Time
}

type Outcome string

const (
	OutcomeProcessed         Outcome = "PROCESSED"
	OutcomeRejected          Outcome = "REJECTED"
	OutcomeAwaitingReference Outcome = "AWAITING_REFERENCE"
)

// Business rejections are decisions, not errors: they are persisted and
// replayed. Apply returns an error only when it cannot decide.
type Decision struct {
	Outcome   Outcome
	Rejection *Rejection
	// nil when no money moved.
	Entry  *ledger.Entry
	Events []event.Payload
}

// Apply checks, in order: transaction and wallet sanity; player and currency;
// the reference (wait if missing or pending); then funds. Player and currency
// come first so a request aimed at the wrong wallet is not left waiting for a
// reference it could never use.
func (t *Transaction) Apply(in ApplyInput) (Decision, error) {
	w := in.Wallet
	switch {
	case t.s.Status.IsTerminal():
		return Decision{}, fmt.Errorf("%w: %s is %s", ErrIllegalTransition, t.s.ID, t.s.Status)
	case w == nil || w.ID() != t.s.WalletID:
		return Decision{}, fmt.Errorf("%w: wallet does not match transaction %s", ErrInvalidTransaction, t.s.ID)
	case t.s.Origin != OriginExternal:
		return Decision{}, fmt.Errorf("%w: only external operations are applied", ErrInvalidTransaction)
	}
	if w.PlayerID() != t.s.PlayerID {
		return t.rejectWith(CodeWalletPlayerMismatch, uuid.Nil, in.Now)
	}
	if w.Currency() != t.s.Money.Currency() {
		return t.rejectWith(CodeCurrencyMismatch, uuid.Nil, in.Now)
	}

	var ref *Transaction
	if t.s.Kind.isReversal() || (t.s.Kind == KindWin && t.s.ReferenceExternalTransactionID != "") {
		r := in.Reference.Tx
		if r == nil || !r.s.Status.IsTerminal() {
			return t.awaitReference(in)
		}
		if code, bad := t.checkReference(r, in.Reference.AlreadyReversed); bad {
			return t.rejectWith(code, r.s.ID, in.Now)
		}
		ref = r
	}

	var mv wallet.Movement
	var err error
	switch t.s.Kind {
	case KindLoss:
		// No movement, no entry, no version bump (§7).
		return t.process(w, nil, nil, in)
	case KindBet:
		mv, err = w.Debit(t.s.Money, in.Now)
		if errors.Is(err, wallet.ErrInsufficientFunds) {
			return t.rejectWith(CodeInsufficientFunds, uuid.Nil, in.Now)
		}
	case KindWin, KindRefund:
		mv, err = w.Credit(t.s.Money, in.Now)
	case KindRollback:
		// The opposite of the original movement.
		if ref.s.Kind == KindBet {
			mv, err = w.Credit(t.s.Money, in.Now)
		} else {
			mv, err = w.Debit(t.s.Money, in.Now)
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				// A distinct code (§7): the reversal, not the bet, cannot be honoured.
				return t.rejectWith(CodeReversalInsufficientFunds, ref.s.ID, in.Now)
			}
		}
	default:
		return Decision{}, fmt.Errorf("%w: kind %s cannot be applied", ErrInvalidTransaction, t.s.Kind)
	}
	if err != nil {
		return Decision{}, fmt.Errorf("apply %s %s: %w", t.s.Kind, t.s.ID, err)
	}
	return t.process(w, &mv, ref, in)
}

func (t *Transaction) checkReference(r *Transaction, alreadyReversed bool) (FailureCode, bool) {
	if r.s.Status != StatusProcessed {
		return CodeReferenceNotProcessed, true // REJECTED or FAILED: definitive
	}
	// §7: operation and reference must agree; the provider is re-checked so the
	// rule does not depend on how the caller searched.
	if r.s.ProviderID != t.s.ProviderID || r.s.PlayerID != t.s.PlayerID || r.s.WalletID != t.s.WalletID ||
		r.s.Money.Currency() != t.s.Money.Currency() || r.s.RoundID != t.s.RoundID {
		return CodeReferenceMismatch, true
	}
	switch t.s.Kind {
	case KindWin, KindRefund:
		if r.s.Kind != KindBet {
			return CodeReferenceKindInvalid, true
		}
	case KindRollback:
		if r.s.Kind != KindBet && r.s.Kind != KindWin && r.s.Kind != KindRefund {
			return CodeReferenceKindInvalid, true // ROLLBACK, LOSS, OPENING
		}
	}
	// No partial reversals; a WIN's amount is unrelated to the bet amount.
	if t.s.Kind.isReversal() && r.s.Money != t.s.Money {
		return CodeReferenceAmountMismatch, true
	}
	// One successful reversal per transaction; a reversed bet cannot win.
	if alreadyReversed {
		return CodeAlreadyReversed, true
	}
	return "", false
}

// awaitReference parks or reschedules the transaction and expires it after
// TTL or MaxAttempts.
func (t *Transaction) awaitReference(in ApplyInput) (Decision, error) {
	p := in.Retry
	if p.TTL <= 0 || p.MaxAttempts < 1 || !p.NextAttemptAt.After(in.Now) {
		return Decision{}, fmt.Errorf("%w: invalid retry policy %+v", ErrInvalidTransaction, p)
	}
	if t.s.Status == StatusPending {
		if err := t.transition(StatusPendingReference, in.Now); err != nil {
			return Decision{}, err
		}
		t.s.Attempts = 1
		t.s.ExpiresAt = in.Now.UTC().Add(p.TTL)
		t.s.NextAttemptAt = p.NextAttemptAt.UTC()
		return Decision{Outcome: OutcomeAwaitingReference, Events: []event.Payload{
			event.WagerTransactionPendingReference{
				TransactionID: t.s.ID, WalletID: t.s.WalletID, PlayerID: t.s.PlayerID, Kind: string(t.s.Kind),
				Money: t.s.Money, ProviderID: t.s.ProviderID, ExternalTransactionID: t.s.ExternalTransactionID,
				ReferenceExternalTransactionID: t.s.ReferenceExternalTransactionID, ExpiresAt: t.s.ExpiresAt,
			},
		}}, nil
	}
	attempts := t.s.Attempts + 1
	if !in.Now.Before(t.s.ExpiresAt) || attempts >= p.MaxAttempts {
		code := CodeReferenceNotFound
		if in.Reference.Tx != nil {
			// It exists but never reached a terminal state in time.
			code = CodeReferenceNotProcessed
		}
		t.s.Attempts = attempts
		return t.rejectWith(code, uuid.Nil, in.Now)
	}
	if err := t.transition(StatusPendingReference, in.Now); err != nil {
		return Decision{}, err
	}
	t.s.Attempts = attempts
	t.s.NextAttemptAt = p.NextAttemptAt.UTC()
	// No event on a retry.
	return Decision{Outcome: OutcomeAwaitingReference}, nil
}

func (t *Transaction) rejectWith(code FailureCode, ref uuid.UUID, now time.Time) (Decision, error) {
	if err := t.reject(code, ref, now); err != nil {
		return Decision{}, err
	}
	return Decision{
		Outcome:   OutcomeRejected,
		Rejection: &Rejection{Code: code},
		Events: []event.Payload{event.WagerTransactionRejected{
			TransactionID: t.s.ID, WalletID: t.s.WalletID, PlayerID: t.s.PlayerID, Kind: string(t.s.Kind),
			Money: t.s.Money, ProviderID: t.s.ProviderID, ExternalTransactionID: t.s.ExternalTransactionID,
			FailureCode: string(code),
		}},
	}, nil
}

func (t *Transaction) process(w *wallet.Wallet, mv *wallet.Movement, ref *Transaction, in ApplyInput) (Decision, error) {
	refID := uuid.Nil
	if ref != nil {
		refID = ref.s.ID
	}
	var entry *ledger.Entry
	if mv != nil {
		e, err := mv.Entry(in.EntryID, w.ID(), t.s.ID, in.Now)
		if err != nil {
			return Decision{}, err
		}
		entry = &e
	}
	if err := t.markProcessed(refID, w.Balance(), w.Version(), in.Now); err != nil {
		return Decision{}, err
	}
	events := []event.Payload{t.processedEvent()}
	if mv != nil {
		events = append(events, event.WalletBalanceChanged{
			WalletID: w.ID(), TransactionID: t.s.ID, Direction: mv.Direction,
			Money: mv.Amount, BalanceBefore: mv.Before, BalanceAfter: mv.After, WalletVersion: mv.Version,
		})
	}
	return Decision{Outcome: OutcomeProcessed, Entry: entry, Events: events}, nil
}
