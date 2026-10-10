package money

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestMarshalJSON(t *testing.T) {
	for _, tt := range []struct {
		m    Money
		want string
	}{
		{brl(t, 2500), `{"amount":"25.00","currency":"BRL"}`},
		{usd(t, 1), `{"amount":"0.01","currency":"USD"}`},
		// Internal negative values (reconciliation difference) serialise too.
		{brl(t, -5), `{"amount":"-0.05","currency":"BRL"}`},
	} {
		got, err := json.Marshal(tt.m)
		if err != nil || string(got) != tt.want {
			t.Errorf("Marshal = %s, %v, want %s", got, err, tt.want)
		}
	}
}

func TestMarshalJSONRejectsZeroValue(t *testing.T) {
	if _, err := json.Marshal(Money{}); !errors.Is(err, ErrZeroValue) {
		t.Fatalf("err = %v, want ErrZeroValue", err)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	var body struct {
		Money Money `json:"money"`
	}
	if err := json.Unmarshal([]byte(`{"money":{"currency":"BRL","amount":"975.00"}}`), &body); err != nil {
		t.Fatal(err)
	}
	if body.Money != brl(t, 97500) {
		t.Fatalf("got %v %v", body.Money.Minor(), body.Money.Currency())
	}
}

func TestUnmarshalJSONIsStrict(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want error
	}{
		// JSON numbers would be decoded through float64 by most code paths;
		// the contract says strings, and anything else is rejected.
		{`{"amount":25.00,"currency":"BRL"}`, ErrInvalidFormat},
		{`{"amount":"25.00"}`, ErrInvalidFormat},
		{`{"currency":"BRL"}`, ErrInvalidFormat},
		{`{"amount":null,"currency":"BRL"}`, ErrInvalidFormat},
		{`{"amount":"25.00","currency":"BRL","extra":1}`, ErrInvalidFormat},
		// encoding/json matches field names case-insensitively; we do not.
		{`{"Amount":"25.00","currency":"BRL"}`, ErrInvalidFormat},
		// Duplicate keys: which one wins would be decoder-dependent.
		{`{"amount":"1.00","amount":"2.00","currency":"BRL"}`, ErrInvalidFormat},
		{`null`, ErrInvalidFormat},
		{`"25.00"`, ErrInvalidFormat},
		{`[]`, ErrInvalidFormat},
		// The amount and currency rules are Parse's.
		{`{"amount":"-1.00","currency":"BRL"}`, ErrNegative},
		{`{"amount":"25.0","currency":"BRL"}`, ErrScale},
		{`{"amount":"1e3","currency":"BRL"}`, ErrInvalidFormat},
		{`{"amount":"1.00","currency":"brl"}`, ErrInvalidCurrency},
		{`{"amount":"92233720368547758.08","currency":"BRL"}`, ErrOverflow},
	} {
		var m Money
		err := json.Unmarshal([]byte(tt.in), &m)
		if !errors.Is(err, tt.want) {
			t.Errorf("Unmarshal(%s) err = %v, want %v", tt.in, err, tt.want)
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	for _, in := range []Money{brl(t, 0), brl(t, 2500), usd(t, maxMinor)} {
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var out Money
		if err := json.Unmarshal(b, &out); err != nil || out != in {
			t.Fatalf("round trip %s -> %v, %v", b, out, err)
		}
	}
}
