package money

import (
	"errors"
	"fmt"
)

var ErrInvalidCurrency = errors.New("money: invalid currency")

// Currency is a supported ISO 4217 code. The unexported field prevents
// building arbitrary currencies outside this package; the zero value means
// "no currency" and is rejected by every Money operation.
type Currency struct{ code string }

// Every supported currency has two minor digits, which is what allows the
// fixed scale of Money. USD exists to test currency mismatch.
var (
	BRL = Currency{code: "BRL"}
	USD = Currency{code: "USD"}
)

var supported = map[string]Currency{
	BRL.code: BRL,
	USD.code: USD,
}

// ParseCurrency does not trim or upper-case its input, so each currency has a
// single accepted spelling and payload hashes need no normalisation.
func ParseCurrency(s string) (Currency, error) {
	if len(s) != 3 {
		return Currency{}, fmt.Errorf("%w: %q is not a 3-letter ISO 4217 code", ErrInvalidCurrency, s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return Currency{}, fmt.Errorf("%w: %q must be upper-case letters", ErrInvalidCurrency, s)
		}
	}
	c, ok := supported[s]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q is not supported", ErrInvalidCurrency, s)
	}
	return c, nil
}

func (c Currency) String() string { return c.code }

func (c Currency) IsZero() bool { return c.code == "" }
