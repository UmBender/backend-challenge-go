package app

import (
	"context"
	"errors"
	"time"

	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

// WagerMetrics reports operation outcomes. Origin is "http", "sqs" or
// "worker"; result is the stored status or an error class.
type WagerMetrics interface {
	WagerResult(origin, result string, replay bool, took time.Duration)
	ConcurrencyConflict()
	PendingRetry()
}

type NopWagerMetrics struct{}

func (NopWagerMetrics) WagerResult(string, string, bool, time.Duration) {}
func (NopWagerMetrics) ConcurrencyConflict()                            {}
func (NopWagerMetrics) PendingRetry()                                   {}

// resultLabel keeps label values bounded: a status, or one error class.
func resultLabel(tx *wager.Transaction, err error) string {
	var inv *wager.InvalidRequestError
	switch {
	case err == nil && tx != nil:
		return string(tx.Status())
	case errors.As(err, &inv):
		return "invalid_request"
	case errors.Is(err, ErrIdempotencyKeyReused), errors.Is(err, ErrExternalTransactionConflict), errors.Is(err, ErrMessageReused):
		return "idempotency_conflict"
	case errors.Is(err, ErrWalletNotFound):
		return "wallet_not_found"
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrConcurrentConflict),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "unavailable"
	default:
		return "error"
	}
}
