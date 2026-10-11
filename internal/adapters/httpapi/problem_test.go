package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
)

var discard = slog.New(slog.DiscardHandler)

type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body %s: %v", rec.Body, err)
	}
	if p.Status != rec.Code || p.Title == "" || p.Type == "" {
		t.Fatalf("incomplete problem %+v for status %d", p, rec.Code)
	}
	return p
}

// TestErrorMapping is the status table of the contract (blueprint §8/§9):
// every class of failure has its own status and stable code.
func TestErrorMapping(t *testing.T) {
	secret := &pgconn.PgError{Code: "08006", Message: "connection to 10.0.0.7 failed"}
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{&wager.InvalidRequestError{Code: wager.CodeInvalidRequest, Reason: "roundId is required"}, 400, "INVALID_REQUEST"},
		{&wager.InvalidRequestError{Code: wager.CodeUnsupportedKind, Reason: "x"}, 400, "UNSUPPORTED_KIND"},
		{&wager.InvalidRequestError{Code: wager.CodeMissingIdempotencyKey, Reason: "x"}, 400, "MISSING_IDEMPOTENCY_KEY"},
		{&wager.InvalidRequestError{Code: wager.CodeInvalidAmountForKind, Reason: "x"}, 400, "INVALID_AMOUNT_FOR_KIND"},
		{fmt.Errorf("decode money: %w", money.ErrScale), 400, "INVALID_MONEY"},
		{fmt.Errorf("decode money: %w", money.ErrInvalidCurrency), 400, "INVALID_MONEY"},
		{errInvalidBody, 400, "INVALID_REQUEST"},
		{wallet.ErrInvalidWallet, 400, "INVALID_REQUEST"},
		{app.ErrInvalidPage, 400, "INVALID_REQUEST"},
		{errProviderMismatch, 403, "PROVIDER_MISMATCH"},
		{app.ErrWalletNotFound, 404, "WALLET_NOT_FOUND"},
		{app.ErrTransactionNotFound, 404, "TRANSACTION_NOT_FOUND"},
		{app.ErrIdempotencyKeyReused, 409, "IDEMPOTENCY_KEY_REUSED"},
		{app.ErrExternalTransactionConflict, 409, "EXTERNAL_TRANSACTION_CONFLICT"},
		{app.ErrWalletAlreadyExists, 409, "WALLET_ALREADY_EXISTS"},
		{fmt.Errorf("%w: %w", app.ErrUnavailable, secret), 503, "SERVICE_UNAVAILABLE"},
		{app.ErrConcurrentConflict, 503, "SERVICE_UNAVAILABLE"},
		{context.DeadlineExceeded, 503, "SERVICE_UNAVAILABLE"},
		{fmt.Errorf("%w: boom", app.ErrInvariantViolation), 500, "INTERNAL_ERROR"},
		{errors.New("something unexpected at 10.0.0.7"), 500, "INTERNAL_ERROR"},
	} {
		rec := httptest.NewRecorder()
		writeError(rec, httptest.NewRequest(http.MethodGet, "/", nil), discard, tt.err)
		if rec.Code != tt.status {
			t.Errorf("%v: status %d, want %d", tt.err, rec.Code, tt.status)
			continue
		}
		p := decodeProblem(t, rec)
		if p.Code != tt.code {
			t.Errorf("%v: code %s, want %s", tt.err, p.Code, tt.code)
		}
		if tt.status == 503 && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%v: 503 without Retry-After", tt.err)
		}
		// Infrastructure details never reach the client.
		if tt.status >= 500 && strings.Contains(rec.Body.String(), "10.0.0.7") {
			t.Errorf("%v: body leaks internals: %s", tt.err, rec.Body)
		}
	}
}

func TestWriteProblemCarriesDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProblem(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusUnauthorized, codeUnauthenticated, "missing bearer token")
	p := decodeProblem(t, rec)
	if p.Code != "UNAUTHENTICATED" || p.Detail != "missing bearer token" || p.Title != "Unauthorized" {
		t.Fatalf("problem = %+v", p)
	}
	_, _ = io.Copy(io.Discard, rec.Body)
}
