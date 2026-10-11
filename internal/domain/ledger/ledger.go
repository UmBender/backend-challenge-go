// Package ledger models the append-only wallet ledger. Each entry stores the
// balance before and after it, so every row can be checked on its own
// (after = before ± amount) and reconciliation finds the exact entry where a
// balance diverged.
package ledger

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)

var (
	// ErrInvalidEntry is a bug, never a business outcome.
	ErrInvalidEntry = errors.New("ledger: invalid entry")
	ErrArithmetic   = errors.New("ledger: balance arithmetic does not hold")
)

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

func ParseDirection(s string) (Direction, error) {
	switch d := Direction(s); d {
	case Debit, Credit:
		return d, nil
	default:
		return "", fmt.Errorf("%w: unknown direction %q", ErrInvalidEntry, s)
	}
}

// Params avoids nine positional arguments of similar types, which are easy
// to swap by accident.
type Params struct {
	ID            uuid.UUID
	WalletID      uuid.UUID
	TransactionID uuid.UUID
	Direction     Direction
	Amount        money.Money
	Before        money.Money
	After         money.Money
	// With UNIQUE (wallet_id, wallet_version), a lost update becomes a
	// constraint violation.
	WalletVersion int64
	CreatedAt     time.Time
}

// Entry is immutable and only built by NewEntry.
type Entry struct{ p Params }

// NewEntry serves both creation and loading: an entry has no side effects,
// so one validating constructor is enough.
func NewEntry(p Params) (Entry, error) {
	if p.ID == uuid.Nil || p.WalletID == uuid.Nil || p.TransactionID == uuid.Nil {
		return Entry{}, fmt.Errorf("%w: missing identifier", ErrInvalidEntry)
	}
	if _, err := ParseDirection(string(p.Direction)); err != nil {
		return Entry{}, err
	}
	if !p.Amount.IsPositive() {
		// LOSS and rejected operations produce no entry (§6.4).
		return Entry{}, fmt.Errorf("%w: amount must be positive, got %s", ErrInvalidEntry, p.Amount)
	}
	cur := p.Amount.Currency()
	if p.Before.Currency() != cur || p.After.Currency() != cur {
		return Entry{}, fmt.Errorf("%w: mixed currencies", ErrInvalidEntry)
	}
	if p.Before.IsNegative() || p.After.IsNegative() {
		return Entry{}, fmt.Errorf("%w: balances cannot be negative", ErrInvalidEntry)
	}
	if p.WalletVersion < 1 {
		return Entry{}, fmt.Errorf("%w: wallet version must be >= 1", ErrInvalidEntry)
	}
	if p.CreatedAt.IsZero() {
		return Entry{}, fmt.Errorf("%w: missing creation time", ErrInvalidEntry)
	}

	var want money.Money
	var err error
	if p.Direction == Credit {
		want, err = p.Before.Add(p.Amount)
	} else {
		want, err = p.Before.Sub(p.Amount)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %v", ErrArithmetic, err)
	}
	if want != p.After {
		return Entry{}, fmt.Errorf("%w: %s %s %s != %s", ErrArithmetic, p.Before, p.Direction, p.Amount, p.After)
	}
	p.CreatedAt = p.CreatedAt.UTC()
	return Entry{p: p}, nil
}

func (e Entry) ID() uuid.UUID            { return e.p.ID }
func (e Entry) WalletID() uuid.UUID      { return e.p.WalletID }
func (e Entry) TransactionID() uuid.UUID { return e.p.TransactionID }
func (e Entry) Direction() Direction     { return e.p.Direction }
func (e Entry) Amount() money.Money      { return e.p.Amount }
func (e Entry) Before() money.Money      { return e.p.Before }
func (e Entry) After() money.Money       { return e.p.After }
func (e Entry) WalletVersion() int64     { return e.p.WalletVersion }
func (e Entry) CreatedAt() time.Time     { return e.p.CreatedAt }
