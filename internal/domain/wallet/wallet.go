// Package wallet implements the Wallet aggregate. Every balance change goes
// through Debit or Credit, which keep the balance non-negative and return the
// Movement to be written to the ledger in the same SQL transaction.
package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)

var (
	ErrInvalidWallet = errors.New("wallet: invalid wallet")
	// A stored wallet that violates an invariant: a bug or tampering.
	ErrCorruptState = errors.New("wallet: corrupt stored state")
	// Mapped to INSUFFICIENT_FUNDS or REVERSAL_INSUFFICIENT_FUNDS by the caller.
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")
	ErrCurrencyMismatch  = errors.New("wallet: currency mismatch")
	// A zero movement would bump the version without changing the balance.
	ErrInvalidAmount = errors.New("wallet: movement amount must be positive")
)

// Snapshot is plain persisted state; Wallet enforces the invariants.
type Snapshot struct {
	ID        uuid.UUID
	PlayerID  uuid.UUID
	Balance   money.Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// The currency is the balance's currency, not a separate field, so the two
// can never disagree.
type Wallet struct{ s Snapshot }

// Movement is one balance change, without the identifiers a ledger entry needs.
type Movement struct {
	Direction ledger.Direction
	Amount    money.Money
	Before    money.Money
	After     money.Money
	Version   int64
}

// New creates a wallet at version 1 with its initial balance (§9). The
// OPENING transaction records that credit in the ledger. Loading from
// storage uses Rehydrate, so it never looks like an opening.
func New(id, playerID uuid.UUID, initial money.Money, now time.Time) (*Wallet, error) {
	switch {
	case id == uuid.Nil || playerID == uuid.Nil:
		return nil, fmt.Errorf("%w: missing identifier", ErrInvalidWallet)
	case !initial.IsValid():
		return nil, fmt.Errorf("%w: missing initial balance", ErrInvalidWallet)
	case initial.IsNegative():
		return nil, fmt.Errorf("%w: initial balance cannot be negative", ErrInvalidWallet)
	case now.IsZero():
		return nil, fmt.Errorf("%w: missing creation time", ErrInvalidWallet)
	}
	now = now.UTC()
	return &Wallet{s: Snapshot{
		ID: id, PlayerID: playerID, Balance: initial, Version: 1, CreatedAt: now, UpdatedAt: now,
	}}, nil
}

// Rehydrate checks invariants only: no movement, no version change, no event.
func Rehydrate(s Snapshot) (*Wallet, error) {
	switch {
	case s.ID == uuid.Nil || s.PlayerID == uuid.Nil:
		return nil, fmt.Errorf("%w: missing identifier", ErrCorruptState)
	case !s.Balance.IsValid():
		return nil, fmt.Errorf("%w: missing balance", ErrCorruptState)
	case s.Balance.IsNegative():
		return nil, fmt.Errorf("%w: negative balance %s", ErrCorruptState, s.Balance)
	case s.Version < 1:
		return nil, fmt.Errorf("%w: version %d < 1", ErrCorruptState, s.Version)
	case s.CreatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt):
		return nil, fmt.Errorf("%w: inconsistent timestamps", ErrCorruptState)
	}
	s.CreatedAt, s.UpdatedAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	return &Wallet{s: s}, nil
}

// On error the wallet is unchanged; on success the version increases by one.
func (w *Wallet) Debit(m money.Money, now time.Time) (Movement, error) {
	if err := w.checkAmount(m); err != nil {
		return Movement{}, err
	}
	after, err := w.s.Balance.Sub(m)
	if err != nil {
		return Movement{}, err
	}
	if after.IsNegative() {
		return Movement{}, fmt.Errorf("%w: balance %s, debit %s", ErrInsufficientFunds, w.s.Balance, m)
	}
	return w.apply(ledger.Debit, m, after, now), nil
}

func (w *Wallet) Credit(m money.Money, now time.Time) (Movement, error) {
	if err := w.checkAmount(m); err != nil {
		return Movement{}, err
	}
	after, err := w.s.Balance.Add(m)
	if err != nil {
		return Movement{}, err
	}
	return w.apply(ledger.Credit, m, after, now), nil
}

func (w *Wallet) checkAmount(m money.Money) error {
	if !m.IsPositive() {
		return fmt.Errorf("%w: got %s", ErrInvalidAmount, m)
	}
	if m.Currency() != w.Currency() {
		return fmt.Errorf("%w: wallet %s, movement %s", ErrCurrencyMismatch, w.Currency(), m.Currency())
	}
	return nil
}

// apply is the only place that mutates the balance.
func (w *Wallet) apply(dir ledger.Direction, amount, after money.Money, now time.Time) Movement {
	mv := Movement{Direction: dir, Amount: amount, Before: w.s.Balance, After: after, Version: w.s.Version + 1}
	w.s.Balance, w.s.Version, w.s.UpdatedAt = after, mv.Version, now.UTC()
	return mv
}

func (mv Movement) Entry(id, walletID, txID uuid.UUID, at time.Time) (ledger.Entry, error) {
	return ledger.NewEntry(ledger.Params{
		ID: id, WalletID: walletID, TransactionID: txID,
		Direction: mv.Direction, Amount: mv.Amount, Before: mv.Before, After: mv.After,
		WalletVersion: mv.Version, CreatedAt: at,
	})
}

func (w *Wallet) Snapshot() Snapshot { return w.s }

func (w *Wallet) ID() uuid.UUID            { return w.s.ID }
func (w *Wallet) PlayerID() uuid.UUID      { return w.s.PlayerID }
func (w *Wallet) Currency() money.Currency { return w.s.Balance.Currency() }
func (w *Wallet) Balance() money.Money     { return w.s.Balance }
func (w *Wallet) Version() int64           { return w.s.Version }
func (w *Wallet) CreatedAt() time.Time     { return w.s.CreatedAt }
func (w *Wallet) UpdatedAt() time.Time     { return w.s.UpdatedAt }
