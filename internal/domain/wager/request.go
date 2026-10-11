package wager

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/domain/money"
)

// The schema repeats this limit, so an oversized value is a 400, not a
// database error.
const maxTextLen = 255

// Request is an operation after transport decoding. The idempotency key is
// not part of it and is excluded from the payload hash (§9).
type Request struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
}

// Validate applies the rules that need no stored state. Rules that need the
// wallet or the reference are decided by Apply and persisted as REJECTED.
func (r Request) Validate() error {
	for _, f := range []struct{ name, value string }{
		{"providerId", r.ProviderID},
		{"externalTransactionId", r.ExternalTransactionID},
		{"roundId", r.RoundID},
		{"gameId", r.GameID},
	} {
		if f.value == "" {
			return invalid(CodeInvalidRequest, f.name+" is required")
		}
		if len(f.value) > maxTextLen {
			return invalid(CodeInvalidRequest, fmt.Sprintf("%s longer than %d bytes", f.name, maxTextLen))
		}
	}
	if r.PlayerID == uuid.Nil || r.WalletID == uuid.Nil {
		return invalid(CodeInvalidRequest, "playerId and walletId are required")
	}
	if _, err := ParseExternalKind(string(r.Kind)); err != nil {
		return err
	}
	if !r.Money.IsValid() {
		return invalid(CodeInvalidMoney, "money is required")
	}
	// Zero policy (§7): LOSS is exactly 0.00, every other external kind > 0.
	if r.Kind == KindLoss && !r.Money.IsZero() {
		return invalid(CodeInvalidAmountForKind, "LOSS must have amount 0.00")
	}
	if r.Kind != KindLoss && !r.Money.IsPositive() {
		return invalid(CodeInvalidAmountForKind, string(r.Kind)+" requires a positive amount")
	}
	ref := r.ReferenceExternalTransactionID
	switch {
	case r.Kind.isReversal() && ref == "":
		return invalid(CodeInvalidRequest, string(r.Kind)+" requires referenceExternalTransactionId")
	case (r.Kind == KindBet || r.Kind == KindLoss) && ref != "":
		// Rejected, not ignored: it would still change the payload hash.
		return invalid(CodeInvalidRequest, string(r.Kind)+" does not take a reference")
	case len(ref) > maxTextLen:
		return invalid(CodeInvalidRequest, "referenceExternalTransactionId too long")
	case ref != "" && ref == r.ExternalTransactionID:
		return invalid(CodeInvalidRequest, "an operation cannot reference itself")
	}
	return nil
}

// The server never derives a key (§9), so an empty one is an error.
func ValidateIdempotencyKey(key string) error {
	if key == "" {
		return invalid(CodeMissingIdempotencyKey, "Idempotency-Key is required")
	}
	if len(key) > maxTextLen {
		return invalid(CodeInvalidRequest, fmt.Sprintf("Idempotency-Key longer than %d bytes", maxTextLen))
	}
	return nil
}
