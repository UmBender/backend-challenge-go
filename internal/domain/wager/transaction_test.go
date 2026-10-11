package wager

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/event"
	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

var (
	t0   = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	hash = sha256.Sum256([]byte("payload"))
)

func newExternal(t *testing.T, r Request) *Transaction {
	t.Helper()
	tx, err := NewExternal(uuid.New(), r, "key-"+r.ExternalTransactionID, hash[:], t0)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestNewExternalStartsPending(t *testing.T) {
	r := validBet(t)
	id := uuid.New()
	tx, err := NewExternal(id, r, "provider-a:transaction-123", hash[:], t0.In(time.FixedZone("X", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	s := tx.Snapshot()
	if s.ID != id || s.Origin != OriginExternal || s.Status != StatusPending || s.Kind != KindBet ||
		s.WalletID != r.WalletID || s.PlayerID != r.PlayerID || s.Money != r.Money ||
		s.ProviderID != r.ProviderID || s.ExternalTransactionID != r.ExternalTransactionID ||
		s.IdempotencyKey != "provider-a:transaction-123" || !bytes.Equal(s.PayloadHash, hash[:]) ||
		s.RoundID != r.RoundID || s.GameID != r.GameID {
		t.Fatalf("snapshot does not reflect the request: %+v", s)
	}
	if s.CreatedAt.Location() != time.UTC || !s.CreatedAt.Equal(t0) || !s.UpdatedAt.Equal(t0) {
		t.Fatalf("timestamps: %v %v", s.CreatedAt, s.UpdatedAt)
	}
	// The hash is copied: a caller reusing its buffer cannot alter the record.
	buf := bytes.Clone(hash[:])
	tx2, _ := NewExternal(uuid.New(), r, "k", buf, t0)
	buf[0] ^= 0xff
	if !bytes.Equal(tx2.Snapshot().PayloadHash, hash[:]) {
		t.Fatal("payload hash aliases the caller's slice")
	}
}

func TestNewExternalRejectsInvalidInput(t *testing.T) {
	r := validBet(t)
	var inv *InvalidRequestError
	if _, err := NewExternal(uuid.New(), Request{}, "k", hash[:], t0); !errors.As(err, &inv) {
		t.Errorf("invalid request: err = %v", err)
	}
	if _, err := NewExternal(uuid.New(), r, "", hash[:], t0); !errors.As(err, &inv) || inv.Code != CodeMissingIdempotencyKey {
		t.Errorf("missing key: err = %v", err)
	}
	for name, call := range map[string]func() error{
		"nil id":     func() error { _, err := NewExternal(uuid.Nil, r, "k", hash[:], t0); return err },
		"short hash": func() error { _, err := NewExternal(uuid.New(), r, "k", hash[:8], t0); return err },
		"no time":    func() error { _, err := NewExternal(uuid.New(), r, "k", hash[:], time.Time{}); return err },
	} {
		if err := call(); !errors.Is(err, ErrInvalidTransaction) {
			t.Errorf("%s: err = %v, want ErrInvalidTransaction", name, err)
		}
	}
}

func TestNewOpeningRecordsTheInitialCredit(t *testing.T) {
	w, err := wallet.New(uuid.New(), uuid.New(), brl(t, "1000.00"), t0)
	if err != nil {
		t.Fatal(err)
	}
	txID, entryID := uuid.New(), uuid.New()
	o, err := NewOpening(txID, entryID, w, t0)
	if err != nil {
		t.Fatal(err)
	}
	s := o.Transaction.Snapshot()
	if s.Origin != OriginInternal || s.Kind != KindOpening || s.Status != StatusProcessed ||
		s.Money != brl(t, "1000.00") || s.WalletID != w.ID() || s.PlayerID != w.PlayerID() {
		t.Fatalf("opening transaction: %+v", s)
	}
	// §6.3: no provider, external id, key, hash, round, game or reference.
	if s.ProviderID != "" || s.ExternalTransactionID != "" || s.IdempotencyKey != "" || s.PayloadHash != nil ||
		s.RoundID != "" || s.GameID != "" || s.ReferenceExternalTransactionID != "" {
		t.Fatalf("opening carries external metadata: %+v", s)
	}
	if s.ResultBalance != brl(t, "1000.00") || s.ResultWalletVersion != 1 || !s.ProcessedAt.Equal(t0) {
		t.Fatalf("opening result: %+v", s)
	}
	e := o.Entry
	if e.ID() != entryID || e.TransactionID() != txID || e.Direction() != ledger.Credit ||
		!e.Before().IsZero() || e.After() != brl(t, "1000.00") || e.WalletVersion() != 1 {
		t.Fatalf("opening entry: %+v", e)
	}
	// §9: WagerTransactionProcessed and WalletBalanceChanged, in that order.
	if len(o.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(o.Events))
	}
	p, ok := o.Events[0].(event.WagerTransactionProcessed)
	if !ok || p.Kind != "OPENING" || p.Balance != brl(t, "1000.00") || p.WalletVersion != 1 || p.ProviderID != "" {
		t.Fatalf("first event: %#v", o.Events[0])
	}
	b, ok := o.Events[1].(event.WalletBalanceChanged)
	if !ok || b.Direction != ledger.Credit || !b.BalanceBefore.IsZero() || b.BalanceAfter != brl(t, "1000.00") || b.WalletVersion != 1 {
		t.Fatalf("second event: %#v", o.Events[1])
	}
}

func TestNewOpeningRefusesZeroOrUsedWallets(t *testing.T) {
	zero, _ := wallet.New(uuid.New(), uuid.New(), brl(t, "0.00"), t0)
	if _, err := NewOpening(uuid.New(), uuid.New(), zero, t0); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("zero balance must not produce an opening: err = %v", err)
	}
	used, _ := wallet.New(uuid.New(), uuid.New(), brl(t, "10.00"), t0)
	_, _ = used.Credit(brl(t, "1.00"), t0)
	if _, err := NewOpening(uuid.New(), uuid.New(), used, t0); !errors.Is(err, ErrInvalidTransaction) {
		t.Errorf("opening needs a version-1 wallet: err = %v", err)
	}
}

func TestRehydrateRoundTrip(t *testing.T) {
	ext := newExternal(t, validBet(t))
	w, _ := wallet.New(uuid.New(), uuid.New(), brl(t, "5.00"), t0)
	o, _ := NewOpening(uuid.New(), uuid.New(), w, t0)
	for _, tx := range []*Transaction{ext, o.Transaction} {
		back, err := Rehydrate(tx.Snapshot())
		if err != nil {
			t.Fatalf("Rehydrate(%s): %v", tx.Snapshot().Kind, err)
		}
		if back.Status() != tx.Status() || back.ID() != tx.ID() {
			t.Fatalf("round trip changed the transaction")
		}
	}
}

func TestRehydrateRejectsCorruptState(t *testing.T) {
	for name, change := range map[string]func(*Snapshot){
		"unknown status":         func(s *Snapshot) { s.Status = "DONE" },
		"external without key":   func(s *Snapshot) { s.IdempotencyKey = "" },
		"external without hash":  func(s *Snapshot) { s.PayloadHash = nil },
		"external opening":       func(s *Snapshot) { s.Kind = KindOpening },
		"loss with amount":       func(s *Snapshot) { s.Kind = KindLoss },
		"processed no result":    func(s *Snapshot) { s.Status = StatusProcessed },
		"rejected no code":       func(s *Snapshot) { s.Status = StatusRejected },
		"nil id":                 func(s *Snapshot) { s.ID = uuid.Nil },
		"zero-value money":       func(s *Snapshot) { s.Money = money.Money{} },
		"refund without ref":     func(s *Snapshot) { s.Kind = KindRefund },
		"internal with provider": func(s *Snapshot) { s.Origin = OriginInternal; s.Kind = KindOpening },
	} {
		s := newExternal(t, validBet(t)).Snapshot()
		change(&s)
		if _, err := Rehydrate(s); !errors.Is(err, ErrCorruptState) {
			t.Errorf("%s: err = %v, want ErrCorruptState", name, err)
		}
	}
}

// TestTransitionMatrix covers all 5 x 5 status pairs (§6.3): terminal states
// accept nothing, nothing returns to PENDING, and PENDING_REFERENCE may
// re-enter itself (a retry).
func TestTransitionMatrix(t *testing.T) {
	all := []Status{StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed}
	allowed := map[[2]Status]bool{
		{StatusPending, StatusPendingReference}:          true,
		{StatusPending, StatusProcessed}:                 true,
		{StatusPending, StatusRejected}:                  true,
		{StatusPending, StatusFailed}:                    true,
		{StatusPendingReference, StatusPendingReference}: true,
		{StatusPendingReference, StatusProcessed}:        true,
		{StatusPendingReference, StatusRejected}:         true,
		{StatusPendingReference, StatusFailed}:           true,
	}
	for _, from := range all {
		for _, to := range all {
			want := allowed[[2]Status{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestFail(t *testing.T) {
	tx := newExternal(t, validBet(t))
	if err := tx.Fail(t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != StatusFailed || tx.FailureCode() != CodeProcessingFailed {
		t.Fatalf("status = %s, code = %s", tx.Status(), tx.FailureCode())
	}
	before := tx.Snapshot()
	if err := tx.Fail(t0.Add(2 * time.Second)); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second Fail: err = %v, want ErrIllegalTransition", err)
	}
	if !tx.Snapshot().UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("a refused transition must not touch the transaction")
	}
}
