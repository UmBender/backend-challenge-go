package wallet

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/ledger"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)

var (
	t0 = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Minute)
)

func brl(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func open(t *testing.T, initial string) *Wallet {
	t.Helper()
	w, err := New(uuid.New(), uuid.New(), brl(t, initial), t0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNewStartsAtVersionOne(t *testing.T) {
	id, player := uuid.New(), uuid.New()
	local := t0.In(time.FixedZone("BRT", -3*3600))
	w, err := New(id, player, brl(t, "100.00"), local)
	if err != nil {
		t.Fatal(err)
	}
	if w.ID() != id || w.PlayerID() != player || w.Currency() != money.BRL ||
		w.Balance() != brl(t, "100.00") || w.Version() != 1 {
		t.Fatalf("unexpected wallet %+v", w.Snapshot())
	}
	if w.CreatedAt().Location() != time.UTC || !w.CreatedAt().Equal(t0) || !w.UpdatedAt().Equal(t0) {
		t.Fatalf("timestamps must be stored in UTC: %v %v", w.CreatedAt(), w.UpdatedAt())
	}
	if z := open(t, "0.00"); z.Version() != 1 || !z.Balance().IsZero() {
		t.Fatalf("zero opening: %+v", z.Snapshot())
	}
}

func TestNewRejectsInvalidInput(t *testing.T) {
	neg, _ := money.FromMinor(-1, money.BRL)
	for name, call := range map[string]func() (*Wallet, error){
		"nil id":        func() (*Wallet, error) { return New(uuid.Nil, uuid.New(), brl(t, "1.00"), t0) },
		"nil player":    func() (*Wallet, error) { return New(uuid.New(), uuid.Nil, brl(t, "1.00"), t0) },
		"zero money":    func() (*Wallet, error) { return New(uuid.New(), uuid.New(), money.Money{}, t0) },
		"negative":      func() (*Wallet, error) { return New(uuid.New(), uuid.New(), neg, t0) },
		"no clock time": func() (*Wallet, error) { return New(uuid.New(), uuid.New(), brl(t, "1.00"), time.Time{}) },
	} {
		if _, err := call(); !errors.Is(err, ErrInvalidWallet) {
			t.Errorf("%s: err = %v, want ErrInvalidWallet", name, err)
		}
	}
}

func TestRehydrateRestoresStateWithoutChangingIt(t *testing.T) {
	s := Snapshot{
		ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, "975.00"),
		Version: 7, CreatedAt: t0, UpdatedAt: t1,
	}
	w, err := Rehydrate(s)
	if err != nil {
		t.Fatal(err)
	}
	if w.Snapshot() != s {
		t.Fatalf("Rehydrate changed state: %+v != %+v", w.Snapshot(), s)
	}
}

func TestRehydrateRejectsCorruptState(t *testing.T) {
	neg, _ := money.FromMinor(-1, money.BRL)
	for name, change := range map[string]func(*Snapshot){
		"version zero":     func(s *Snapshot) { s.Version = 0 },
		"negative balance": func(s *Snapshot) { s.Balance = neg },
		"zero-value money": func(s *Snapshot) { s.Balance = money.Money{} },
		"nil id":           func(s *Snapshot) { s.ID = uuid.Nil },
		"nil player":       func(s *Snapshot) { s.PlayerID = uuid.Nil },
		"updated < created": func(s *Snapshot) {
			s.UpdatedAt = s.CreatedAt.Add(-time.Second)
		},
	} {
		s := Snapshot{ID: uuid.New(), PlayerID: uuid.New(), Balance: brl(t, "1.00"), Version: 1, CreatedAt: t0, UpdatedAt: t0}
		change(&s)
		if _, err := Rehydrate(s); !errors.Is(err, ErrCorruptState) {
			t.Errorf("%s: err = %v, want ErrCorruptState", name, err)
		}
	}
}

func TestDebit(t *testing.T) {
	w := open(t, "100.00")
	mv, err := w.Debit(brl(t, "80.00"), t1)
	if err != nil {
		t.Fatal(err)
	}
	want := Movement{Direction: ledger.Debit, Amount: brl(t, "80.00"), Before: brl(t, "100.00"), After: brl(t, "20.00"), Version: 2}
	if mv != want {
		t.Fatalf("movement = %+v, want %+v", mv, want)
	}
	if w.Balance() != brl(t, "20.00") || w.Version() != 2 || !w.UpdatedAt().Equal(t1) {
		t.Fatalf("wallet after debit: %+v", w.Snapshot())
	}
}

// TestFailedDebitChangesNothing is the domain half of the 80+80 scenario:
// the second bet sees 20.00 and is refused without touching the version.
func TestFailedDebitChangesNothing(t *testing.T) {
	w := open(t, "100.00")
	if _, err := w.Debit(brl(t, "80.00"), t1); err != nil {
		t.Fatal(err)
	}
	before := w.Snapshot()
	if _, err := w.Debit(brl(t, "80.00"), t1.Add(time.Second)); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if w.Snapshot() != before {
		t.Fatalf("failed debit mutated the wallet: %+v", w.Snapshot())
	}
}

func TestDebitWholeBalance(t *testing.T) {
	w := open(t, "100.00")
	if _, err := w.Debit(brl(t, "100.00"), t1); err != nil || !w.Balance().IsZero() {
		t.Fatalf("balance = %v, err = %v", w.Balance(), err)
	}
}

func TestCredit(t *testing.T) {
	w := open(t, "0.00")
	mv, err := w.Credit(brl(t, "50.00"), t1)
	if err != nil {
		t.Fatal(err)
	}
	if mv.Direction != ledger.Credit || mv.After != brl(t, "50.00") || mv.Version != 2 || w.Version() != 2 {
		t.Fatalf("movement = %+v, wallet = %+v", mv, w.Snapshot())
	}
}

func TestCreditOverflowChangesNothing(t *testing.T) {
	w := open(t, "92233720368547758.07")
	before := w.Snapshot()
	if _, err := w.Credit(brl(t, "0.01"), t1); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("err = %v, want money.ErrOverflow", err)
	}
	if w.Snapshot() != before {
		t.Fatal("failed credit mutated the wallet")
	}
}

func TestMovementsRejectInvalidAmounts(t *testing.T) {
	usd, _ := money.Parse("1.00", "USD")
	for name, tt := range map[string]struct {
		m    money.Money
		want error
	}{
		"zero":        {brl(t, "0.00"), ErrInvalidAmount},
		"zero value":  {money.Money{}, ErrInvalidAmount},
		"other money": {usd, ErrCurrencyMismatch},
	} {
		w := open(t, "100.00")
		if _, err := w.Debit(tt.m, t1); !errors.Is(err, tt.want) {
			t.Errorf("debit %s: err = %v, want %v", name, err, tt.want)
		}
		if _, err := w.Credit(tt.m, t1); !errors.Is(err, tt.want) {
			t.Errorf("credit %s: err = %v, want %v", name, err, tt.want)
		}
		if w.Version() != 1 {
			t.Errorf("%s: version changed without a balance change", name)
		}
	}
}

func TestMovementBuildsALedgerEntry(t *testing.T) {
	w := open(t, "100.00")
	mv, _ := w.Debit(brl(t, "25.00"), t1)
	entryID, txID := uuid.New(), uuid.New()
	e, err := mv.Entry(entryID, w.ID(), txID, t1)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID() != entryID || e.WalletID() != w.ID() || e.TransactionID() != txID ||
		e.After() != w.Balance() || e.WalletVersion() != w.Version() {
		t.Fatalf("entry does not match the movement: %+v", e)
	}
}
