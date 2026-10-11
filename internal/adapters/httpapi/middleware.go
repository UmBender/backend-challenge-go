package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

const (
	correlationHeader = "X-Correlation-Id"
	maxCorrelationLen = 128
	requestIDKey      = ctxKey(iota + 200)
	requestInfoKey    = ctxKey(iota + 201)
)

// withCorrelation propagates or creates X-Correlation-Id and gives each
// request its own id (the causation id of events). An invalid header is
// replaced, not rejected, so it can never inject log lines.
func withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(correlationHeader)
		if !validCorrelation(id) {
			id = uuid.NewString()
		}
		w.Header().Set(correlationHeader, id)
		ctx := context.WithValue(r.Context(), correlationKey, id)
		ctx = context.WithValue(ctx, requestIDKey, uuid.NewString())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validCorrelation(s string) bool {
	if s == "" || len(s) > maxCorrelationLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e { // printable ASCII, no spaces
			return false
		}
	}
	return true
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// The deadline reaches the database through the context: pgx cancels the
// running query when it expires.
func withTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requestInfo is filled by authentication and read by the access log.
type requestInfo struct {
	clientID   string
	providerID string
}

func requestInfoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey).(*requestInfo)
	return info
}

// withAccessLog never logs bodies or headers: they hold amounts, player ids
// and bearer tokens (§12).
func withAccessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{}
		r = r.WithContext(context.WithValue(r.Context(), requestInfoKey, info))
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		log.InfoContext(r.Context(), "http request",
			"method", r.Method, "route", route, "status", rec.code,
			"durationMs", time.Since(start).Milliseconds(),
			"correlationId", correlationID(r.Context()), "requestId", requestID(r.Context()),
			"clientId", info.clientID, "providerId", info.providerID)
	})
}
