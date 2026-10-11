package wager

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/event"
	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

var (
	// A constructor received unusable input: a bug.
	ErrInvalidTransaction = errors.New("wager: invalid transaction")
	ErrCorruptState       = errors.New("wager: corrupt stored state")
	ErrIllegalTransition  = errors.New("wager: illegal state transition")
)

const hashSize = 32

// Snapshot maps one field per column. Zero values mean "not applicable".
type Snapshot struct {
	ID                             uuid.UUID
	Origin                         Origin
	Kind                           Kind
	Status                         Status
	WalletID                       uuid.UUID
	PlayerID                       uuid.UUID
	Money                          money.Money
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    []byte
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
	ReferenceTransactionID         uuid.UUID
	FailureCode                    FailureCode
	// Outcome at processing time; replays return it, never the current balance (§9).
	ResultBalance       money.Money
	ResultWalletVersion int64
	// Retry state lives in the row, not in memory, so any instance can resume.
	Attempts      int
	NextAttemptAt time.Time
	ExpiresAt     time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ProcessedAt   time.Time
}

// All state changes go through transition methods guarded by the state machine.
type Transaction struct{ s Snapshot }

// NewExternal validates the request, so every external transaction goes
// through the same rules.
func NewExternal(id uuid.UUID, r Request, key string, hash []byte, now time.Time) (*Transaction, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateIdempotencyKey(key); err != nil {
		return nil, err
	}
	switch {
	case id == uuid.Nil:
		return nil, fmt.Errorf("%w: missing id", ErrInvalidTransaction)
	case len(hash) != hashSize:
		return nil, fmt.Errorf("%w: payload hash must be %d bytes, got %d", ErrInvalidTransaction, hashSize, len(hash))
	case now.IsZero():
		return nil, fmt.Errorf("%w: missing creation time", ErrInvalidTransaction)
	}
	now = now.UTC()
	return &Transaction{s: Snapshot{
		ID: id, Origin: OriginExternal, Kind: r.Kind, Status: StatusPending,
		WalletID: r.WalletID, PlayerID: r.PlayerID, Money: r.Money,
		ProviderID: r.ProviderID, ExternalTransactionID: r.ExternalTransactionID,
		IdempotencyKey: key, PayloadHash: bytes.Clone(hash), // never alias the caller's buffer
		RoundID: r.RoundID, GameID: r.GameID,
		ReferenceExternalTransactionID: r.ReferenceExternalTransactionID,
		CreatedAt:                      now, UpdatedAt: now,
	}}, nil
}

// Opening is what a positive wallet opening commits together with the wallet.
type Opening struct {
	Transaction *Transaction
	Entry       ledger.Entry
	Events      []event.Payload
}

// NewOpening credits from 0.00 to the initial balance at wallet version 1, so
// reconciliation includes the opening. A zero balance has no opening (§9).
func NewOpening(id, entryID uuid.UUID, w *wallet.Wallet, now time.Time) (Opening, error) {
	switch {
	case id == uuid.Nil || w == nil:
		return Opening{}, fmt.Errorf("%w: missing id or wallet", ErrInvalidTransaction)
	case w.Version() != 1:
		return Opening{}, fmt.Errorf("%w: opening needs a new wallet (version 1), got %d", ErrInvalidTransaction, w.Version())
	case !w.Balance().IsPositive():
		return Opening{}, fmt.Errorf("%w: a zero opening balance has no OPENING transaction", ErrInvalidTransaction)
	}
	zero, err := money.Zero(w.Currency())
	if err != nil {
		return Opening{}, err
	}
	entry, err := wallet.Movement{
		Direction: ledger.Credit, Amount: w.Balance(), Before: zero, After: w.Balance(), Version: 1,
	}.Entry(entryID, w.ID(), id, now)
	if err != nil {
		return Opening{}, err
	}
	now = now.UTC()
	tx := &Transaction{s: Snapshot{
		ID: id, Origin: OriginInternal, Kind: KindOpening, Status: StatusPending,
		WalletID: w.ID(), PlayerID: w.PlayerID(), Money: w.Balance(),
		CreatedAt: now, UpdatedAt: now,
	}}
	// Through the state machine, so a single code path sets results.
	if err := tx.markProcessed(uuid.Nil, w.Balance(), 1, now); err != nil {
		return Opening{}, err
	}
	return Opening{
		Transaction: tx,
		Entry:       entry,
		Events: []event.Payload{
			tx.processedEvent(),
			event.WalletBalanceChanged{
				WalletID: w.ID(), TransactionID: id, Direction: ledger.Credit,
				Money: w.Balance(), BalanceBefore: zero, BalanceAfter: w.Balance(), WalletVersion: 1,
			},
		},
	}, nil
}

// Rehydrate re-checks the rules the schema enforces, catching hand-edited
// rows, and never emits events.
func Rehydrate(s Snapshot) (*Transaction, error) {
	if err := checkShape(s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	s.PayloadHash = bytes.Clone(s.PayloadHash)
	return &Transaction{s: s}, nil
}

func checkShape(s Snapshot) error {
	if s.ID == uuid.Nil || s.WalletID == uuid.Nil || s.PlayerID == uuid.Nil {
		return errors.New("missing identifier")
	}
	if _, err := ParseKind(string(s.Kind)); err != nil {
		return err
	}
	if _, err := ParseStatus(string(s.Status)); err != nil {
		return err
	}
	if !s.Money.IsValid() || s.Money.IsNegative() {
		return errors.New("invalid amount")
	}
	external := s.ProviderID != "" || s.ExternalTransactionID != "" || s.IdempotencyKey != "" ||
		s.PayloadHash != nil || s.RoundID != "" || s.GameID != "" || s.ReferenceExternalTransactionID != ""
	switch s.Origin {
	case OriginInternal:
		if s.Kind != KindOpening || external {
			return errors.New("internal transactions are OPENING without provider metadata")
		}
	case OriginExternal:
		if s.Kind == KindOpening || s.ProviderID == "" || s.ExternalTransactionID == "" ||
			s.IdempotencyKey == "" || len(s.PayloadHash) != hashSize || s.RoundID == "" || s.GameID == "" {
			return errors.New("external transactions need complete provider metadata")
		}
	default:
		return fmt.Errorf("unknown origin %q", s.Origin)
	}
	if (s.Kind == KindLoss) != s.Money.IsZero() {
		return errors.New("only LOSS has a zero amount")
	}
	if s.Kind.isReversal() && s.ReferenceExternalTransactionID == "" {
		return errors.New("reversal without reference")
	}
	if (s.Status == StatusRejected || s.Status == StatusFailed) && s.FailureCode == "" {
		return errors.New("terminal failure without code")
	}
	if s.Status == StatusProcessed && !s.ResultBalance.IsValid() {
		return errors.New("processed without result")
	}
	return nil
}

// allowed is the state machine (§6.3): terminal states have no outgoing
// transitions, nothing returns to PENDING, PENDING_REFERENCE → itself is a retry.
var allowed = map[Status][]Status{
	StatusPending:          {StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed},
	StatusPendingReference: {StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed},
}

func CanTransition(from, to Status) bool {
	for _, s := range allowed[from] {
		if s == to {
			return true
		}
	}
	return false
}

// transition is the single gate for status changes; a refusal changes nothing.
func (t *Transaction) transition(to Status, now time.Time) error {
	if !CanTransition(t.s.Status, to) {
		return fmt.Errorf("%w: %s -> %s (transaction %s)", ErrIllegalTransition, t.s.Status, to, t.s.ID)
	}
	t.s.Status, t.s.UpdatedAt = to, now.UTC()
	return nil
}

func (t *Transaction) markProcessed(ref uuid.UUID, balance money.Money, version int64, now time.Time) error {
	if err := t.transition(StatusProcessed, now); err != nil {
		return err
	}
	t.s.ReferenceTransactionID = ref
	t.s.ResultBalance, t.s.ResultWalletVersion = balance, version
	t.s.ProcessedAt = now.UTC()
	t.s.NextAttemptAt = time.Time{} // no longer due for the worker
	return nil
}

func (t *Transaction) reject(code FailureCode, ref uuid.UUID, now time.Time) error {
	if err := t.transition(StatusRejected, now); err != nil {
		return err
	}
	t.s.FailureCode, t.s.ReferenceTransactionID = code, ref
	t.s.ProcessedAt = now.UTC()
	t.s.NextAttemptAt = time.Time{}
	return nil
}

// Fail records a permanent non-business failure for audit; only asynchronous
// paths call it.
func (t *Transaction) Fail(now time.Time) error {
	if err := t.transition(StatusFailed, now); err != nil {
		return err
	}
	t.s.FailureCode = CodeProcessingFailed
	t.s.ProcessedAt = now.UTC()
	t.s.NextAttemptAt = time.Time{}
	return nil
}

func (t *Transaction) processedEvent() event.WagerTransactionProcessed {
	var ref *uuid.UUID
	if t.s.ReferenceTransactionID != uuid.Nil {
		r := t.s.ReferenceTransactionID
		ref = &r
	}
	return event.WagerTransactionProcessed{
		TransactionID: t.s.ID, WalletID: t.s.WalletID, PlayerID: t.s.PlayerID, Kind: string(t.s.Kind),
		Money: t.s.Money, ProviderID: t.s.ProviderID, ExternalTransactionID: t.s.ExternalTransactionID,
		RoundID: t.s.RoundID, GameID: t.s.GameID, ReferenceTransactionID: ref,
		Balance: t.s.ResultBalance, WalletVersion: t.s.ResultWalletVersion,
	}
}

func (t *Transaction) Snapshot() Snapshot {
	s := t.s
	s.PayloadHash = bytes.Clone(s.PayloadHash)
	return s
}

func (t *Transaction) ID() uuid.UUID                     { return t.s.ID }
func (t *Transaction) Kind() Kind                        { return t.s.Kind }
func (t *Transaction) Status() Status                    { return t.s.Status }
func (t *Transaction) WalletID() uuid.UUID               { return t.s.WalletID }
func (t *Transaction) PlayerID() uuid.UUID               { return t.s.PlayerID }
func (t *Transaction) Money() money.Money                { return t.s.Money }
func (t *Transaction) ProviderID() string                { return t.s.ProviderID }
func (t *Transaction) ExternalTransactionID() string     { return t.s.ExternalTransactionID }
func (t *Transaction) IdempotencyKey() string            { return t.s.IdempotencyKey }
func (t *Transaction) PayloadHash() []byte               { return bytes.Clone(t.s.PayloadHash) }
func (t *Transaction) RoundID() string                   { return t.s.RoundID }
func (t *Transaction) ReferenceExternalID() string       { return t.s.ReferenceExternalTransactionID }
func (t *Transaction) ReferenceTransactionID() uuid.UUID { return t.s.ReferenceTransactionID }
func (t *Transaction) FailureCode() FailureCode          { return t.s.FailureCode }
func (t *Transaction) ResultBalance() money.Money        { return t.s.ResultBalance }
func (t *Transaction) ResultWalletVersion() int64        { return t.s.ResultWalletVersion }
func (t *Transaction) Attempts() int                     { return t.s.Attempts }
func (t *Transaction) ExpiresAt() time.Time              { return t.s.ExpiresAt }
func (t *Transaction) NextAttemptAt() time.Time          { return t.s.NextAttemptAt }
