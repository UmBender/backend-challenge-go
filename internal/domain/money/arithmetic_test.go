package money

import (
	"errors"
	"testing"
)

func brl(t *testing.T, minor int64) Money {
	t.Helper()
	m, err := FromMinor(minor, BRL)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func usd(t *testing.T, minor int64) Money {
	t.Helper()
	m, err := FromMinor(minor, USD)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestZero(t *testing.T) {
	z, err := Zero(BRL)
	if err != nil || z.Minor() != 0 || z.Currency() != BRL || !z.IsZero() {
		t.Fatalf("Zero(BRL) = %v %v, %v", z.Minor(), z.Currency(), err)
	}
	if _, err := Zero(Currency{}); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("Zero(Currency{}) err = %v, want ErrInvalidCurrency", err)
	}
}

func TestAddSubNeg(t *testing.T) {
	sum, err := brl(t, 10000).Add(brl(t, 2550))
	if err != nil || sum != brl(t, 12550) {
		t.Fatalf("100.00 + 25.50 = %v, %v", sum, err)
	}
	diff, err := brl(t, 2000).Sub(brl(t, 8000))
	if err != nil || diff != brl(t, -6000) || !diff.IsNegative() {
		t.Fatalf("20.00 - 80.00 = %v, %v (negative differences are allowed)", diff, err)
	}
	neg, err := brl(t, 2500).Neg()
	if err != nil || neg != brl(t, -2500) {
		t.Fatalf("-(25.00) = %v, %v", neg, err)
	}
}

func TestArithmeticDoesNotMutateOperands(t *testing.T) {
	a, b := brl(t, 100), brl(t, 50)
	_, _ = a.Add(b)
	_, _ = a.Sub(b)
	_, _ = a.Neg()
	if a != brl(t, 100) || b != brl(t, 50) {
		t.Fatal("Money must be immutable")
	}
}

func TestArithmeticOverflow(t *testing.T) {
	for _, tt := range []struct {
		name string
		op   func() (Money, error)
	}{
		{"max + 1", func() (Money, error) { return brl(t, maxMinor).Add(brl(t, 1)) }},
		{"min + -1", func() (Money, error) { return brl(t, minMinor).Add(brl(t, -1)) }},
		{"min - 1", func() (Money, error) { return brl(t, minMinor).Sub(brl(t, 1)) }},
		{"max - -1", func() (Money, error) { return brl(t, maxMinor).Sub(brl(t, -1)) }},
		// 0 - MinInt64 would need +2^63; computing it as 0 + (-MinInt64)
		// would overflow inside Neg first, so Sub must check directly.
		{"0 - min", func() (Money, error) { return brl(t, 0).Sub(brl(t, minMinor)) }},
		// Two's complement asymmetry: -MinInt64 does not exist.
		{"neg min", func() (Money, error) { return brl(t, minMinor).Neg() }},
	} {
		if _, err := tt.op(); !errors.Is(err, ErrOverflow) {
			t.Errorf("%s: err = %v, want ErrOverflow", tt.name, err)
		}
	}
	// The boundaries themselves are fine.
	if m, err := brl(t, maxMinor).Neg(); err != nil || m.Minor() != -maxMinor {
		t.Fatalf("neg max = %v, %v", m, err)
	}
	if m, err := brl(t, maxMinor-1).Add(brl(t, 1)); err != nil || m.Minor() != maxMinor {
		t.Fatalf("max-1 + 1 = %v, %v", m, err)
	}
}

func TestCurrencyMismatch(t *testing.T) {
	a, b := brl(t, 100), usd(t, 100)
	if _, err := a.Add(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add err = %v", err)
	}
	if _, err := a.Sub(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub err = %v", err)
	}
	if _, err := a.Cmp(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp err = %v", err)
	}
}

func TestCmp(t *testing.T) {
	for _, tt := range []struct {
		a, b int64
		want int
	}{
		{100, 200, -1},
		{200, 100, 1},
		{100, 100, 0},
		{-1, 0, -1},
	} {
		got, err := brl(t, tt.a).Cmp(brl(t, tt.b))
		if err != nil || got != tt.want {
			t.Errorf("Cmp(%d, %d) = %d, %v, want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
}

func TestPredicates(t *testing.T) {
	if !brl(t, 0).IsZero() || brl(t, 1).IsZero() {
		t.Error("IsZero")
	}
	if !brl(t, -1).IsNegative() || brl(t, 0).IsNegative() {
		t.Error("IsNegative")
	}
	if !brl(t, 1).IsPositive() || brl(t, 0).IsPositive() {
		t.Error("IsPositive")
	}
}

// TestZeroValueIsRejected covers §6: an uninitialised Money{} must never be
// usable as an amount, on either side of any operation.
func TestZeroValueIsRejected(t *testing.T) {
	var invalid Money
	valid := brl(t, 100)
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"invalid.Add", second(invalid.Add(valid))},
		{"valid.Add(invalid)", second(valid.Add(invalid))},
		{"invalid.Sub", second(invalid.Sub(valid))},
		{"valid.Sub(invalid)", second(valid.Sub(invalid))},
		{"invalid.Neg", second(invalid.Neg())},
		{"invalid.Cmp", second(invalid.Cmp(valid))},
		{"valid.Cmp(invalid)", second(valid.Cmp(invalid))},
	} {
		if !errors.Is(tt.err, ErrZeroValue) {
			t.Errorf("%s err = %v, want ErrZeroValue", tt.name, tt.err)
		}
	}
	// Predicates cannot return an error; they answer false so that a check
	// such as "amount.IsPositive()" never passes for an uninitialised value.
	if invalid.IsZero() || invalid.IsNegative() || invalid.IsPositive() {
		t.Error("predicates must be false for the zero value")
	}
	if invalid.IsValid() || !valid.IsValid() {
		t.Error("IsValid")
	}
}

func second[T any](_ T, err error) error { return err }
