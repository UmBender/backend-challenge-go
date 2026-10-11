package app

import (
	"errors"
	"testing"

	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

func TestResultLabelIsBounded(t *testing.T) {
	for want, err := range map[string]error{
		"invalid_request":      &wager.InvalidRequestError{Code: wager.CodeInvalidRequest},
		"idempotency_conflict": ErrIdempotencyKeyReused,
		"wallet_not_found":     ErrWalletNotFound,
		"unavailable":          ErrConcurrentConflict,
		"error":                errors.New("something new"),
	} {
		if got := resultLabel(nil, err); got != want {
			t.Errorf("resultLabel(%v) = %q, want %q", err, got, want)
		}
	}
}
