package money

import (
	"errors"
	"testing"
)

func TestParseCurrencyAcceptsSupportedCodes(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want Currency
	}{
		{"BRL", BRL},
		{"USD", USD},
	} {
		got, err := ParseCurrency(tt.in)
		if err != nil {
			t.Fatalf("ParseCurrency(%q): %v", tt.in, err)
		}
		if got != tt.want || got.String() != tt.in {
			t.Fatalf("ParseCurrency(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseCurrencyRejectsEverythingElse(t *testing.T) {
	for _, in := range []string{
		"",     // missing
		"brl",  // ISO 4217 codes are upper case; we do not normalise
		"Brl",  // idem
		"BR",   // too short
		"BRLL", // too long
		" BRL", // no trimming: transport layers must not send padding
		"B1L",  // letters only
		"JPY",  // well-formed but unsupported (zero minor digits, see Currency)
	} {
		if _, err := ParseCurrency(in); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("ParseCurrency(%q) err = %v, want ErrInvalidCurrency", in, err)
		}
	}
}

func TestZeroCurrencyIsInvalid(t *testing.T) {
	var c Currency
	if !c.IsZero() {
		t.Fatal("the zero Currency must report IsZero")
	}
	if BRL.IsZero() {
		t.Fatal("BRL must not be zero")
	}
}
