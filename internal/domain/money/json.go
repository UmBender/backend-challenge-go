package money

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MarshalJSON writes {"amount":"25.00","currency":"BRL"}. The amount is a
// string, never a JSON number, because most clients decode numbers as floats.
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, ErrZeroValue
	}
	b := make([]byte, 0, 48)
	b = append(b, `{"amount":"`...)
	b = append(b, formatMinor(m.minor)...)
	b = append(b, `","currency":"`...)
	b = append(b, m.currency.code...)
	b = append(b, `"}`...)
	return b, nil
}

// UnmarshalJSON is the boundary for external input, so it applies Parse.
// It reads token by token instead of decoding into a struct, because
// encoding/json matches keys case-insensitively, lets duplicate keys win
// silently and cannot tell a missing field from an empty one.
func (m *Money) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return fmt.Errorf("%w: money must be a JSON object", ErrInvalidFormat)
	}
	fields := map[string]*string{"amount": nil, "currency": nil}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidFormat, err)
		}
		key, _ := tok.(string)
		seen, known := fields[key]
		if !known {
			return fmt.Errorf("%w: unknown money field %q", ErrInvalidFormat, key)
		}
		if seen != nil {
			return fmt.Errorf("%w: duplicate money field %q", ErrInvalidFormat, key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidFormat, err)
		}
		// Only strings: a JSON number is rejected without being converted to a float.
		if len(raw) == 0 || raw[0] != '"' {
			return fmt.Errorf("%w: money %s must be a JSON string", ErrInvalidFormat, key)
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidFormat, err)
		}
		fields[key] = &s
	}
	if fields["amount"] == nil || fields["currency"] == nil {
		return fmt.Errorf("%w: money requires both amount and currency", ErrInvalidFormat)
	}
	parsed, err := Parse(*fields["amount"], *fields["currency"])
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
