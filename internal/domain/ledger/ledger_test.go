package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

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

func validParams(t *testing.T) Params {
	return Params{
		ID:            uuid.New(),
		WalletID:      uuid.New(),
		TransactionID: uuid.New(),
		Direction:     Debit,
		Amount:        brl(t, "25.00"),
		Before:        brl(t, "1000.00"),
		After:         brl(t, "975.00"),
		WalletVersion: 2,
		CreatedAt:     time.Date(2026, 9, 8, 9, 0, 0, 0, time.FixedZone("BRT", -3*3600)),
	}
}

func TestNewEntryDebitAndCredit(t *testing.T) {
	p := validParams(t)
	e, err := NewEntry(p)
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	if e.Direction() != Debit || e.Amount() != p.Amount || e.Before() != p.Before || e.After() != p.After ||
		e.WalletVersion() != 2 || e.ID() != p.ID || e.WalletID() != p.WalletID || e.TransactionID() != p.TransactionID {
		t.Fatalf("accessors do not return the inputs: %+v", e)
	}
	if e.CreatedAt().Location() != time.UTC || !e.CreatedAt().Equal(p.CreatedAt) {
		t.Fatalf("CreatedAt = %v, want the same instant in UTC", e.CreatedAt())
	}

	p.Direction, p.Before, p.After = Credit, brl(t, "975.00"), brl(t, "1000.00")
	if _, err := NewEntry(p); err != nil {
		t.Fatalf("credit: %v", err)
	}
}

func TestNewEntryRejectsWrongArithmetic(t *testing.T) {
	for name, change := range map[string]func(*Params){
		"debit that adds":       func(p *Params) { p.After = brl(t, "1025.00") },
		"credit that subtracts": func(p *Params) { p.Direction = Credit },
		"off by one cent":       func(p *Params) { p.After = brl(t, "975.01") },
	} {
		p := validParams(t)
		change(&p)
		if _, err := NewEntry(p); !errors.Is(err, ErrArithmetic) {
			t.Errorf("%s: err = %v, want ErrArithmetic", name, err)
		}
	}
}

func TestNewEntryRejectsInvalidFields(t *testing.T) {
	usd, _ := money.Parse("25.00", "USD")
	for name, change := range map[string]func(*Params){
		"nil id":            func(p *Params) { p.ID = uuid.Nil },
		"nil wallet":        func(p *Params) { p.WalletID = uuid.Nil },
		"nil transaction":   func(p *Params) { p.TransactionID = uuid.Nil },
		"unknown direction": func(p *Params) { p.Direction = "SIDEWAYS" },
		"zero amount":       func(p *Params) { p.Amount, p.After = brl(t, "0.00"), p.Before },
		"zero-value amount": func(p *Params) { p.Amount = money.Money{} },
		"currency mismatch": func(p *Params) { p.Amount = usd },
		"negative after": func(p *Params) {
			p.Before, p.After = brl(t, "10.00"), mustFromMinor(t, -1500)
		},
		"version zero":  func(p *Params) { p.WalletVersion = 0 },
		"no created at": func(p *Params) { p.CreatedAt = time.Time{} },
	} {
		p := validParams(t)
		change(&p)
		if _, err := NewEntry(p); !errors.Is(err, ErrInvalidEntry) {
			t.Errorf("%s: err = %v, want ErrInvalidEntry", name, err)
		}
	}
}

func TestParseDirection(t *testing.T) {
	for _, s := range []string{"DEBIT", "CREDIT"} {
		if d, err := ParseDirection(s); err != nil || string(d) != s {
			t.Errorf("ParseDirection(%q) = %q, %v", s, d, err)
		}
	}
	if _, err := ParseDirection("debit"); !errors.Is(err, ErrInvalidEntry) {
		t.Errorf("lower case must be rejected, err = %v", err)
	}
}

func mustFromMinor(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
