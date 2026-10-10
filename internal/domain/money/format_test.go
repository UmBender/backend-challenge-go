package money

import (
	"errors"
	"testing"
)

const minMinor int64 = -1 << 63

func TestStringFormatsTwoDecimals(t *testing.T) {
	for _, tt := range []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{10, "0.10"},
		{2500, "25.00"},
		{97500, "975.00"},
		{maxMinor, "92233720368547758.07"},
		// Negative values exist internally (reconciliation differences).
		{-5, "-0.05"},
		{-100, "-1.00"},
		{-12345, "-123.45"},
		// MinInt64 has no positive counterpart; formatting must not negate it.
		{minMinor, "-92233720368547758.08"},
	} {
		m, err := FromMinor(tt.minor, BRL)
		if err != nil {
			t.Fatalf("FromMinor(%d): %v", tt.minor, err)
		}
		if got := m.String(); got != tt.want {
			t.Errorf("FromMinor(%d).String() = %q, want %q", tt.minor, got, tt.want)
		}
	}
}

func TestFromMinorRejectsZeroCurrency(t *testing.T) {
	if _, err := FromMinor(100, Currency{}); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("err = %v, want ErrInvalidCurrency", err)
	}
}

func TestZeroValueStringIsNotAnAmount(t *testing.T) {
	// A forgotten initialisation must not print as "0.00" in logs or bodies.
	if got := (Money{}).String(); got == "0.00" || got == "" {
		t.Fatalf("Money{}.String() = %q, must not look like a valid amount", got)
	}
}

// FuzzParse checks that every accepted string formats back to itself: Parse
// performs no hidden normalisation, so the amount the provider sent is the
// amount we hash, store and echo.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"0.00", "0.01", "25.00", "92233720368547758.07", "25", "01.00", "-1.00", "1e3", "92233720368547758.08"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := Parse(s, "BRL")
		if err != nil {
			return
		}
		if m.Minor() < 0 {
			t.Fatalf("Parse(%q) produced a negative amount %d", s, m.Minor())
		}
		if got := m.String(); got != s {
			t.Fatalf("round trip: %q -> %q", s, got)
		}
	})
}

// FuzzFormat checks the other direction: every non-negative int64 formats to
// a string that Parse accepts and maps back to the same minor units.
func FuzzFormat(f *testing.F) {
	for _, n := range []int64{0, 1, 99, 100, 2500, maxMinor} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, n int64) {
		if n < 0 {
			return
		}
		m, err := FromMinor(n, BRL)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Parse(m.String(), "BRL")
		if err != nil || back.Minor() != n {
			t.Fatalf("FromMinor(%d).String() = %q, Parse = %d, %v", n, m.String(), back.Minor(), err)
		}
	})
}
