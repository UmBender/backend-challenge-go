// Package event defines the integration events published through the
// outbox (§11): one Go type per event, wrapped in an Envelope. Payloads are
// snapshots of values, so the stored JSON is exactly what gets published,
// as many times as needed, with the same eventId.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)

var ErrInvalidEvent = errors.New("event: invalid event")

type Type string

const (
	TypeWagerTransactionProcessed        Type = "WagerTransactionProcessed"
	TypeWagerTransactionRejected         Type = "WagerTransactionRejected"
	TypeWalletBalanceChanged             Type = "WalletBalanceChanged"
	TypeWagerTransactionPendingReference Type = "WagerTransactionPendingReference"
)

// Payload has unexported methods, so only this package defines event types.
type Payload interface {
	eventType() Type
	eventVersion() int
	walletID() uuid.UUID
}

type WalletBalanceChanged struct {
	WalletID      uuid.UUID        `json:"walletId"`
	TransactionID uuid.UUID        `json:"transactionId"`
	Direction     ledger.Direction `json:"direction"`
	Money         money.Money      `json:"money"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	// Lets consumers detect stale or reordered balance events.
	WalletVersion int64 `json:"walletVersion"`
}

// Provider fields are omitted for OPENING, which has no provider.
type WagerTransactionProcessed struct {
	TransactionID          uuid.UUID   `json:"transactionId"`
	WalletID               uuid.UUID   `json:"walletId"`
	PlayerID               uuid.UUID   `json:"playerId"`
	Kind                   string      `json:"kind"`
	Money                  money.Money `json:"money"`
	ProviderID             string      `json:"providerId,omitempty"`
	ExternalTransactionID  string      `json:"externalTransactionId,omitempty"`
	RoundID                string      `json:"roundId,omitempty"`
	GameID                 string      `json:"gameId,omitempty"`
	ReferenceTransactionID *uuid.UUID  `json:"referenceTransactionId,omitempty"`
	Balance                money.Money `json:"balance"`
	WalletVersion          int64       `json:"walletVersion"`
}

// Includes the expiry of a pending reference (REFERENCE_NOT_FOUND).
type WagerTransactionRejected struct {
	TransactionID         uuid.UUID   `json:"transactionId"`
	WalletID              uuid.UUID   `json:"walletId"`
	PlayerID              uuid.UUID   `json:"playerId"`
	Kind                  string      `json:"kind"`
	Money                 money.Money `json:"money"`
	ProviderID            string      `json:"providerId"`
	ExternalTransactionID string      `json:"externalTransactionId"`
	FailureCode           string      `json:"failureCode"`
}

// Emitted once, when the wait starts; retries are silent.
type WagerTransactionPendingReference struct {
	TransactionID                  uuid.UUID   `json:"transactionId"`
	WalletID                       uuid.UUID   `json:"walletId"`
	PlayerID                       uuid.UUID   `json:"playerId"`
	Kind                           string      `json:"kind"`
	Money                          money.Money `json:"money"`
	ProviderID                     string      `json:"providerId"`
	ExternalTransactionID          string      `json:"externalTransactionId"`
	ReferenceExternalTransactionID string      `json:"referenceExternalTransactionId"`
	ExpiresAt                      time.Time   `json:"expiresAt"`
}

func (WalletBalanceChanged) eventType() Type             { return TypeWalletBalanceChanged }
func (WagerTransactionProcessed) eventType() Type        { return TypeWagerTransactionProcessed }
func (WagerTransactionRejected) eventType() Type         { return TypeWagerTransactionRejected }
func (WagerTransactionPendingReference) eventType() Type { return TypeWagerTransactionPendingReference }

// A breaking payload change gets a new version.
func (WalletBalanceChanged) eventVersion() int             { return 1 }
func (WagerTransactionProcessed) eventVersion() int        { return 1 }
func (WagerTransactionRejected) eventVersion() int         { return 1 }
func (WagerTransactionPendingReference) eventVersion() int { return 1 }

func (p WalletBalanceChanged) walletID() uuid.UUID             { return p.WalletID }
func (p WagerTransactionProcessed) walletID() uuid.UUID        { return p.WalletID }
func (p WagerTransactionRejected) walletID() uuid.UUID         { return p.WalletID }
func (p WagerTransactionPendingReference) walletID() uuid.UUID { return p.WalletID }

// Meta comes from the application layer, which knows the request and
// generates identifiers.
type Meta struct {
	EventID       uuid.UUID
	CorrelationID string
	// Optional: SQS messageId or HTTP request id.
	CausationID string
	OccurredAt  time.Time
}

type Envelope struct {
	id          uuid.UUID
	typ         Type
	version     int
	aggregateID uuid.UUID
	correlation string
	causation   string
	occurredAt  time.Time
	data        Payload
}

// New uses the wallet as aggregate: events of one wallet share a FIFO
// message group and are delivered in order.
func New(m Meta, p Payload) (Envelope, error) {
	switch {
	case p == nil:
		return Envelope{}, fmt.Errorf("%w: missing payload", ErrInvalidEvent)
	case m.EventID == uuid.Nil:
		return Envelope{}, fmt.Errorf("%w: missing event id", ErrInvalidEvent)
	case m.OccurredAt.IsZero():
		return Envelope{}, fmt.Errorf("%w: missing occurrence time", ErrInvalidEvent)
	case m.CorrelationID == "":
		return Envelope{}, fmt.Errorf("%w: missing correlation id", ErrInvalidEvent)
	case p.walletID() == uuid.Nil:
		return Envelope{}, fmt.Errorf("%w: payload without wallet id", ErrInvalidEvent)
	}
	return Envelope{
		id: m.EventID, typ: p.eventType(), version: p.eventVersion(), aggregateID: p.walletID(),
		correlation: m.CorrelationID, causation: m.CausationID, occurredAt: m.OccurredAt.UTC(), data: p,
	}, nil
}

func (e Envelope) ID() uuid.UUID          { return e.id }
func (e Envelope) Type() Type             { return e.typ }
func (e Envelope) Version() int           { return e.version }
func (e Envelope) AggregateID() uuid.UUID { return e.aggregateID }
func (e Envelope) CorrelationID() string  { return e.correlation }
func (e Envelope) CausationID() string    { return e.causation }
func (e Envelope) OccurredAt() time.Time  { return e.occurredAt }
func (e Envelope) Data() Payload          { return e.data }

type wire struct {
	EventID       uuid.UUID `json:"eventId"`
	EventType     Type      `json:"eventType"`
	Version       int       `json:"version"`
	AggregateID   uuid.UUID `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Data          Payload   `json:"data"`
}

// An invalid Money fails here, so a broken event is never committed.
func (e Envelope) MarshalJSON() ([]byte, error) {
	return json.Marshal(wire{
		EventID: e.id, EventType: e.typ, Version: e.version, AggregateID: e.aggregateID,
		CorrelationID: e.correlation, CausationID: e.causation, OccurredAt: e.occurredAt, Data: e.data,
	})
}
