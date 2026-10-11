package event

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var (
	walletID = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	txID     = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	playerID = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	eventID  = uuid.MustParse("0192f2a0-0000-7000-8000-000000000001")
)

func meta() Meta {
	return Meta{
		EventID:       eventID,
		CorrelationID: "provider-a:transaction-123",
		CausationID:   "msg-123",
		// Not UTC on purpose: New must normalise.
		OccurredAt: time.Date(2026, 9, 8, 9, 0, 0, 123_000_000, time.FixedZone("BRT", -3*3600)),
	}
}

func TestNewTakesTypeAndVersionFromThePayload(t *testing.T) {
	for _, tt := range []struct {
		p    Payload
		want Type
	}{
		{WagerTransactionProcessed{TransactionID: txID, WalletID: walletID}, TypeWagerTransactionProcessed},
		{WagerTransactionRejected{TransactionID: txID, WalletID: walletID}, TypeWagerTransactionRejected},
		{WalletBalanceChanged{TransactionID: txID, WalletID: walletID}, TypeWalletBalanceChanged},
		{WagerTransactionPendingReference{TransactionID: txID, WalletID: walletID}, TypeWagerTransactionPendingReference},
	} {
		e, err := New(meta(), tt.p)
		if err != nil {
			t.Fatalf("%s: %v", tt.want, err)
		}
		if e.Type() != tt.want || e.Version() != 1 {
			t.Errorf("type/version = %s/%d, want %s/1", e.Type(), e.Version(), tt.want)
		}
		// Decision 17 of "Open questions": every event is keyed by wallet, so
		// FIFO message groups preserve per-wallet order.
		if e.AggregateID() != walletID {
			t.Errorf("%s: aggregateId = %s, want the wallet id", tt.want, e.AggregateID())
		}
		if e.OccurredAt().Location() != time.UTC {
			t.Errorf("occurredAt must be UTC, got %v", e.OccurredAt())
		}
	}
}

func TestNewRejectsIncompleteMetadata(t *testing.T) {
	p := WalletBalanceChanged{WalletID: walletID, TransactionID: txID}
	for name, change := range map[string]func(*Meta){
		"nil event id":      func(m *Meta) { m.EventID = uuid.Nil },
		"no occurred at":    func(m *Meta) { m.OccurredAt = time.Time{} },
		"no correlation id": func(m *Meta) { m.CorrelationID = "" },
	} {
		m := meta()
		change(&m)
		if _, err := New(m, p); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: err = %v, want ErrInvalidEvent", name, err)
		}
	}
	if _, err := New(meta(), nil); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("nil payload: err = %v", err)
	}
	if _, err := New(meta(), WalletBalanceChanged{TransactionID: txID}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("payload without wallet id: err = %v", err)
	}
}

// TestWalletBalanceChangedJSON pins the wire contract of §11 with the example
// of the blueprint: field names, string amounts, UTC RFC 3339 timestamp.
func TestWalletBalanceChangedJSON(t *testing.T) {
	e, err := New(meta(), WalletBalanceChanged{
		WalletID:      walletID,
		TransactionID: txID,
		Direction:     ledger.Debit,
		Money:         brl(t, "25.00"),
		BalanceBefore: brl(t, "1000.00"),
		BalanceAfter:  brl(t, "975.00"),
		WalletVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"eventId":"0192f2a0-0000-7000-8000-000000000001","eventType":"WalletBalanceChanged","version":1,` +
		`"aggregateId":"0192f291-27dd-7d3f-8071-5f8685deef37","correlationId":"provider-a:transaction-123",` +
		`"causationId":"msg-123","occurredAt":"2026-09-08T12:00:00.123Z","data":{` +
		`"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","transactionId":"0192f298-345e-7e38-af88-e43f851a819d",` +
		`"direction":"DEBIT","money":{"amount":"25.00","currency":"BRL"},` +
		`"balanceBefore":{"amount":"1000.00","currency":"BRL"},"balanceAfter":{"amount":"975.00","currency":"BRL"},` +
		`"walletVersion":2}}`
	if string(got) != want {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestCausationIDIsOptional(t *testing.T) {
	m := meta()
	m.CausationID = ""
	e, err := New(m, WagerTransactionRejected{
		TransactionID: txID, WalletID: walletID, PlayerID: playerID,
		Kind: "BET", Money: brl(t, "80.00"), FailureCode: "INSUFFICIENT_FUNDS",
		ProviderID: "provider-a", ExternalTransactionID: "tx-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["causationId"]; ok {
		t.Fatalf("empty causationId must be omitted: %s", got)
	}
}

func TestMarshalFailsOnInvalidMoney(t *testing.T) {
	e, err := New(meta(), WalletBalanceChanged{WalletID: walletID, TransactionID: txID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(e); !errors.Is(err, money.ErrZeroValue) {
		t.Fatalf("err = %v, want money.ErrZeroValue (an event never carries a missing amount)", err)
	}
}
