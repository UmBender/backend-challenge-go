// Package money implements Money: an exact amount stored as int64 minor units
// (centavos). No floating point is used in parsing, arithmetic, formatting or
// JSON; every operation that would leave the int64 range returns ErrOverflow.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrInvalidFormat = errors.New("money: invalid amount format")
	ErrScale         = errors.New("money: amount must have exactly two decimal places")
	ErrNegative      = errors.New("money: negative amount not allowed")
	ErrOverflow      = errors.New("money: amount out of range")
)

const scale = 2

// Spelled out so this package does not import "math".
const (
	maxInt64 = 1<<63 - 1
	minInt64 = -1 << 63
)

// Money is immutable and only built through Parse, FromMinor and Zero.
// The zero value Money{} has no currency and is rejected by every operation.
type Money struct {
	minor    int64
	currency Currency
}

// Parse accepts only ^(0|[1-9][0-9]*)\.[0-9]{2}$: exactly two decimals, no
// sign, no leading zeros. Values are never rounded or padded, and each amount
// has a single string form. Digits are converted by hand, never via floats.
func Parse(amount, currency string) (Money, error) {
	cur, err := ParseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	if strings.HasPrefix(amount, "-") {
		// A well-formed negative number gets a precise error; anything else is malformed.
		if _, err := parseUnsignedMinor(amount[1:]); err == nil {
			return Money{}, fmt.Errorf("%w: %q", ErrNegative, amount)
		}
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidFormat, amount)
	}
	minor, err := parseUnsignedMinor(amount)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", err, amount)
	}
	return Money{minor: minor, currency: cur}, nil
}

func parseUnsignedMinor(s string) (int64, error) {
	intPart, frac, hasDot := strings.Cut(s, ".")
	if !allDigits(intPart) || (len(intPart) > 1 && intPart[0] == '0') {
		return 0, ErrInvalidFormat
	}
	if hasDot && !allDigits(frac) {
		return 0, ErrInvalidFormat
	}
	if len(frac) != scale {
		return 0, ErrScale
	}
	var v int64
	for _, c := range []byte(intPart + frac) {
		d := int64(c - '0')
		// v*10 + d > maxInt64, checked without overflowing.
		if v > (maxInt64-d)/10 {
			return 0, ErrOverflow
		}
		v = v*10 + d
	}
	return v, nil
}

// allDigits checks ASCII bytes, so non-ASCII digits are rejected.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (m Money) Minor() int64 { return m.minor }

func (m Money) Currency() Currency { return m.currency }

// FromMinor builds Money from trusted minor units (database, domain
// arithmetic). It accepts negative values: non-negative balances are an
// invariant of Wallet and of the database, not of Money.
func FromMinor(minor int64, cur Currency) (Money, error) {
	if cur.IsZero() {
		return Money{}, fmt.Errorf("%w: missing currency", ErrInvalidCurrency)
	}
	return Money{minor: minor, currency: cur}, nil
}

// String returns the amount with two decimals ("25.00"). The invalid zero
// value prints a marker instead of "0.00" so it stands out in logs.
func (m Money) String() string {
	if m.currency.IsZero() {
		return "<invalid money>"
	}
	return formatMinor(m.minor)
}

func formatMinor(v int64) string {
	neg := v < 0
	u := uint64(v)
	if neg {
		// -v overflows for MinInt64; this form does not.
		u = uint64(-(v + 1)) + 1
	}
	frac := u % 100
	b := make([]byte, 0, 24)
	if neg {
		b = append(b, '-')
	}
	b = strconv.AppendUint(b, u/100, 10)
	b = append(b, '.', byte('0'+frac/10), byte('0'+frac%10))
	return string(b)
}
