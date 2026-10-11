package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

// The largest legitimate body is well under 2 KiB.
const maxBodyBytes = 64 << 10

var (
	errUnsupportedMediaType = errors.New("Content-Type must be application/json")
	errBodyTooLarge         = errors.New("request body too large")
)

type fields map[string]json.RawMessage

// decodeObject reads one JSON object strictly. encoding/json matches names
// case-insensitively, lets duplicates win silently and ignores trailing data;
// walking the top-level tokens gives exact names, no duplicates and a single
// value.
func decodeObject(w http.ResponseWriter, r *http.Request, allowed ...string) (fields, error) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return nil, errUnsupportedMediaType
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errBodyTooLarge
		}
		return nil, fmt.Errorf("%w: %v", errInvalidBody, err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("%w: the body must be one JSON object", errInvalidBody)
	}
	out := fields{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalidBody, err)
		}
		name, _ := tok.(string)
		if !contains(allowed, name) {
			return nil, fmt.Errorf("%w: unknown field %q", errInvalidBody, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%w: duplicate field %q", errInvalidBody, name)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%w: %v", errInvalidBody, err)
		}
		out[name] = raw
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return nil, fmt.Errorf("%w: %v", errInvalidBody, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: data after the JSON object", errInvalidBody)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// str checks only the JSON type; presence and length are domain rules.
func (f fields) str(name string) (string, error) {
	raw, ok := f[name]
	if !ok {
		return "", nil
	}
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", fmt.Errorf("%w: %s must be a string", errInvalidBody, name)
	}
	return s, nil
}

// uuid accepts only the canonical lower-case form, so an identifier has a
// single spelling, like the one used in the hash.
func (f fields) uuid(name string) (uuid.UUID, error) {
	s, err := f.str(name)
	if err != nil || s == "" {
		return uuid.Nil, err
	}
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil, fmt.Errorf("%w: %s must be a lower-case UUID", errInvalidBody, name)
	}
	return id, nil
}

// Absent → Money{}, which the domain rejects as INVALID_MONEY.
func (f fields) money(name string) (money.Money, error) {
	raw, ok := f[name]
	if !ok {
		return money.Money{}, nil
	}
	var m money.Money
	if err := json.Unmarshal(raw, &m); err != nil {
		return money.Money{}, fmt.Errorf("%s: %w", name, err)
	}
	return m, nil
}

func decodeSubmit(w http.ResponseWriter, r *http.Request) (wager.Request, error) {
	f, err := decodeObject(w, r, "providerId", "externalTransactionId", "playerId", "walletId",
		"roundId", "gameId", "kind", "money", "referenceExternalTransactionId")
	if err != nil {
		return wager.Request{}, err
	}
	var req wager.Request
	var kind string
	for _, s := range []struct {
		name string
		dst  *string
	}{
		{"providerId", &req.ProviderID}, {"externalTransactionId", &req.ExternalTransactionID},
		{"roundId", &req.RoundID}, {"gameId", &req.GameID}, {"kind", &kind},
		{"referenceExternalTransactionId", &req.ReferenceExternalTransactionID},
	} {
		if *s.dst, err = f.str(s.name); err != nil {
			return wager.Request{}, err
		}
	}
	req.Kind = wager.Kind(kind)
	if req.PlayerID, err = f.uuid("playerId"); err != nil {
		return wager.Request{}, err
	}
	if req.WalletID, err = f.uuid("walletId"); err != nil {
		return wager.Request{}, err
	}
	if req.Money, err = f.money("money"); err != nil {
		return wager.Request{}, err
	}
	return req, nil
}

func decodeOpenWallet(w http.ResponseWriter, r *http.Request) (app.OpenWalletCommand, error) {
	f, err := decodeObject(w, r, "playerId", "initialBalance")
	if err != nil {
		return app.OpenWalletCommand{}, err
	}
	var cmd app.OpenWalletCommand
	if cmd.PlayerID, err = f.uuid("playerId"); err != nil {
		return cmd, err
	}
	if cmd.PlayerID == uuid.Nil {
		return cmd, fmt.Errorf("%w: playerId is required", errInvalidBody)
	}
	if cmd.InitialBalance, err = f.money("initialBalance"); err != nil {
		return cmd, err
	}
	if !cmd.InitialBalance.IsValid() {
		return cmd, fmt.Errorf("%w: initialBalance is required", errInvalidBody)
	}
	return cmd, nil
}
