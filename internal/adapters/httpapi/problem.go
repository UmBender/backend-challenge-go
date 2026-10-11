package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

var (
	errInvalidBody      = errors.New("invalid request body")
	errProviderMismatch = errors.New("provider mismatch")
)

// Codes that do not come from the domain; domain codes are written as they are.
const (
	codeUnauthenticated      = "UNAUTHENTICATED"
	codeForbidden            = "FORBIDDEN"
	codeProviderMismatch     = "PROVIDER_MISMATCH"
	codeInvalidRequest       = "INVALID_REQUEST"
	codeInvalidMoney         = "INVALID_MONEY"
	codeWalletNotFound       = "WALLET_NOT_FOUND"
	codeTransactionNotFound  = "TRANSACTION_NOT_FOUND"
	codeKeyReused            = "IDEMPOTENCY_KEY_REUSED"
	codeExternalConflict     = "EXTERNAL_TRANSACTION_CONFLICT"
	codeWalletAlreadyExists  = "WALLET_ALREADY_EXISTS"
	codeServiceUnavailable   = "SERVICE_UNAVAILABLE"
	codeInternalError        = "INTERNAL_ERROR"
	codeUnsupportedMediaType = "UNSUPPORTED_MEDIA_TYPE"
	codePayloadTooLarge      = "PAYLOAD_TOO_LARGE"
	retryAfterSeconds        = "1"
	problemContentType       = "application/problem+json"
	problemTypeBlank         = "about:blank"
)

// problem is an RFC 9457 Problem Details object. Clients switch on the "code"
// extension member, the stable contract.
type problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Code          string `json:"code"`
	CorrelationID string `json:"correlationId,omitempty"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
	w.Header().Set("Content-Type", problemContentType)
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusServiceUnavailable {
		// Safe to retry with the same Idempotency-Key.
		w.Header().Set("Retry-After", retryAfterSeconds)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type: problemTypeBlank, Title: http.StatusText(status), Status: status,
		Detail: detail, Code: code, CorrelationID: correlationID(r.Context()),
	})
}

// writeError is the single place where errors become HTTP responses. Details
// are returned for correctable input only; the cause of a 5xx goes to the log.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var inv *wager.InvalidRequestError
	switch {
	case errors.As(err, &inv):
		writeProblem(w, r, http.StatusBadRequest, string(inv.Code), inv.Reason)
	case errors.Is(err, money.ErrInvalidFormat), errors.Is(err, money.ErrScale), errors.Is(err, money.ErrNegative),
		errors.Is(err, money.ErrOverflow), errors.Is(err, money.ErrInvalidCurrency):
		writeProblem(w, r, http.StatusBadRequest, codeInvalidMoney, err.Error())
	case errors.Is(err, errInvalidBody), errors.Is(err, wallet.ErrInvalidWallet), errors.Is(err, app.ErrInvalidPage):
		writeProblem(w, r, http.StatusBadRequest, codeInvalidRequest, err.Error())
	case errors.Is(err, errUnsupportedMediaType):
		writeProblem(w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType, err.Error())
	case errors.Is(err, errBodyTooLarge):
		writeProblem(w, r, http.StatusRequestEntityTooLarge, codePayloadTooLarge, err.Error())
	case errors.Is(err, errProviderMismatch):
		writeProblem(w, r, http.StatusForbidden, codeProviderMismatch, "providerId does not match the authenticated provider")
	case errors.Is(err, app.ErrWalletNotFound):
		writeProblem(w, r, http.StatusNotFound, codeWalletNotFound, "")
	case errors.Is(err, app.ErrTransactionNotFound):
		writeProblem(w, r, http.StatusNotFound, codeTransactionNotFound, "")
	case errors.Is(err, app.ErrIdempotencyKeyReused):
		writeProblem(w, r, http.StatusConflict, codeKeyReused, "the Idempotency-Key was already used with a different payload")
	case errors.Is(err, app.ErrExternalTransactionConflict):
		writeProblem(w, r, http.StatusConflict, codeExternalConflict, "externalTransactionId already exists under another Idempotency-Key")
	case errors.Is(err, app.ErrWalletAlreadyExists):
		writeProblem(w, r, http.StatusConflict, codeWalletAlreadyExists, "the player already has a wallet in this currency")
	case errors.Is(err, app.ErrUnavailable), errors.Is(err, app.ErrConcurrentConflict),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// ErrConcurrentConflict arrives only after the use case's retries.
		log.WarnContext(r.Context(), "request failed transiently", "err", err)
		writeProblem(w, r, http.StatusServiceUnavailable, codeServiceUnavailable, "temporarily unavailable, retry with the same Idempotency-Key")
	default:
		log.ErrorContext(r.Context(), "request failed", "err", err)
		writeProblem(w, r, http.StatusInternalServerError, codeInternalError, "")
	}
}

type ctxKey int

const correlationKey ctxKey = iota

func correlationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey).(string)
	return id
}
