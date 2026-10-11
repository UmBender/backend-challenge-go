// Package app holds the use cases and the ports they need. It imports
// neither pgx, net/http nor Fx: adapters implement the ports.
package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"

	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

// PayloadHash is the SHA-256 of the canonical business payload: sorted keys,
// compact JSON, no HTML escaping. The idempotency key and transport metadata
// are excluded, so HTTP and SQS hash identically, and the hash is computed
// from the validated Request, so whitespace and key order cannot cause
// false conflicts.
func PayloadHash(r wager.Request) ([]byte, error) {
	m := map[string]any{
		"providerId":            r.ProviderID,
		"externalTransactionId": r.ExternalTransactionID,
		"playerId":              r.PlayerID.String(),
		"walletId":              r.WalletID.String(),
		"roundId":               r.RoundID,
		"gameId":                r.GameID,
		"kind":                  string(r.Kind),
		"money": map[string]string{
			"amount":   r.Money.String(),
			"currency": r.Money.Currency().String(),
		},
	}
	if r.ReferenceExternalTransactionID != "" {
		m["referenceExternalTransactionId"] = r.ReferenceExternalTransactionID
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return sum[:], nil
}
