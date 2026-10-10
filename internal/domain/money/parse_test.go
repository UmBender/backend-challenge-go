package money

import (
	"errors"
	"testing"
)

// maxMinor mirrors the int64 limit without importing package math, which the
// no-float rule bans from money code (tests included, to keep grep simple).
const maxMinor int64 = 1<<63 - 1

func TestParseValidAmounts(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.10", 10},
		{"1.00", 100},
		{"25.00", 2500},
		{"10.50", 1050},
		{"1000.00", 100000},
		// The largest representable amount: MaxInt64 minor units.
		{"92233720368547758.07", maxMinor},
	} {
		m, err := Parse(tt.in, "BRL")
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.in, err)
		}
		if m.Minor() != tt.want || m.Currency() != BRL {
			t.Fatalf("Parse(%q) = %d %v, want %d BRL", tt.in, m.Minor(), m.Currency(), tt.want)
		}
	}
}

func TestParseRejectsInvalidAmounts(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want error
	}{
		{"", ErrInvalidFormat},
		// Scale: exactly two decimals, never padded or rounded (decision 1 of
		// "Open questions": "25" and "25.0" are rejected, not normalised).
		{"25", ErrScale},
		{"25.0", ErrScale},
		{"25.000", ErrScale},
		{"25.005", ErrScale}, // must not be rounded to 25.01 or 25.00
		{"25.", ErrInvalidFormat},
		{".50", ErrInvalidFormat},
		// Sign: negative external input is its own error; '+' is not allowed.
		{"-1.00", ErrNegative},
		{"-0.00", ErrNegative},
		{"+1.00", ErrInvalidFormat},
		{"--1.00", ErrInvalidFormat},
		// Things float parsers would accept.
		{"1e3", ErrInvalidFormat},
		{"1E3", ErrInvalidFormat},
		{"1.00e2", ErrInvalidFormat},
		{"NaN", ErrInvalidFormat},
		{"Infinity", ErrInvalidFormat},
		{"inf", ErrInvalidFormat},
		{"0x10.00", ErrInvalidFormat},
		// Formatting variants.
		{" 1.00", ErrInvalidFormat},
		{"1.00 ", ErrInvalidFormat},
		{"1,00", ErrInvalidFormat},
		{"1_000.00", ErrInvalidFormat},
		{"1.0a", ErrInvalidFormat},
		{"１.00", ErrInvalidFormat}, // full-width digit
		// Leading zeros are rejected (decision 2): one canonical form per value.
		{"01.00", ErrInvalidFormat},
		{"00.00", ErrInvalidFormat},
		// One minor unit beyond MaxInt64, and far beyond.
		{"92233720368547758.08", ErrOverflow},
		{"99999999999999999999.00", ErrOverflow},
	} {
		_, err := Parse(tt.in, "BRL")
		if !errors.Is(err, tt.want) {
			t.Errorf("Parse(%q) err = %v, want %v", tt.in, err, tt.want)
		}
	}
}

func TestParseRejectsInvalidCurrency(t *testing.T) {
	if _, err := Parse("1.00", "brl"); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("err = %v, want ErrInvalidCurrency", err)
	}
}
