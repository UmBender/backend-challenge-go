package httpapi

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type staticAuth struct {
	p   Principal
	err error
}

func (s staticAuth) Authenticate(*http.Request) (Principal, error) { return s.p, s.err }

func TestCorrelationIDIsPropagatedOrGenerated(t *testing.T) {
	var seen string
	h := withCorrelation(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = correlationID(r.Context())
	}))
	for name, tt := range map[string]struct {
		header   string
		generate bool
	}{
		"propagated":   {"provider-a:transaction-123", false},
		"absent":       {"", true},
		"too long":     {strings.Repeat("x", 129), true},
		"control char": {"abc\ndef", true},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tt.header != "" {
			r.Header.Set("X-Correlation-Id", tt.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if seen == "" || rec.Header().Get("X-Correlation-Id") != seen {
			t.Errorf("%s: context %q, response header %q", name, seen, rec.Header().Get("X-Correlation-Id"))
		}
		if (seen != tt.header) != tt.generate {
			t.Errorf("%s: correlation id %q (generated=%v)", name, seen, tt.generate)
		}
	}
}

func TestRequestTimeoutBoundsTheContext(t *testing.T) {
	var deadline time.Time
	h := withTimeout(50*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, _ = r.Context().Deadline()
	}))
	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if deadline.IsZero() || deadline.Sub(start) > 100*time.Millisecond {
		t.Fatalf("deadline %v not within the timeout", deadline)
	}
}

func TestAuthorizationPolicy(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, found := principalFrom(r.Context())
		if !found || p.ProviderID != "provider-a" {
			t.Error("principal not in context")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	provider := Principal{ClientID: "provider-a", ProviderID: "provider-a", Roles: []string{RoleWagerSubmit, RoleWagerRead}}
	for name, tt := range map[string]struct {
		auth   Authenticator
		role   string
		status int
		code   string
	}{
		"no credentials":   {staticAuth{err: ErrUnauthenticated}, RoleWagerSubmit, 401, "UNAUTHENTICATED"},
		"deny all":         {DenyAll{}, RoleWagerSubmit, 401, "UNAUTHENTICATED"},
		"missing role":     {staticAuth{p: provider}, RoleWalletAdmin, 403, "FORBIDDEN"},
		"authorised":       {staticAuth{p: provider}, RoleWagerSubmit, 204, ""},
		"verifier failure": {staticAuth{err: errors.New("jwks fetch failed")}, RoleWagerSubmit, 401, "UNAUTHENTICATED"},
	} {
		rec := httptest.NewRecorder()
		require(tt.auth, discard, tt.role, ok).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != tt.status {
			t.Errorf("%s: status %d, want %d", name, rec.Code, tt.status)
			continue
		}
		if tt.code != "" {
			if p := decodeProblem(t, rec); p.Code != tt.code {
				t.Errorf("%s: code %s", name, p.Code)
			}
		}
		if tt.status == 401 && !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: 401 without WWW-Authenticate: Bearer", name)
		}
	}
}

// TestAccessLogHasIdentifiersButNoPayload (§12): who, what, outcome — never
// the body (amounts, player ids).
func TestAccessLogHasIdentifiersButNoPayload(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	mux := http.NewServeMux()
	auth := staticAuth{p: Principal{ClientID: "provider-a", ProviderID: "provider-a", Roles: []string{RoleWagerSubmit}}}
	mux.Handle("POST /wagering/transactions", require(auth, log, RoleWagerSubmit, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})))
	h := withCorrelation(withAccessLog(log, mux))
	r := httptest.NewRequest(http.MethodPost, "/wagering/transactions", strings.NewReader(`{"money":{"amount":"12345.67"}}`))
	r.Header.Set("X-Correlation-Id", "corr-42")
	r.Header.Set("Authorization", "Bearer secret-token")
	h.ServeHTTP(httptest.NewRecorder(), r)
	line := buf.String()
	for _, want := range []string{`"msg":"http request"`, `"route":"POST /wagering/transactions"`, `"status":201`,
		`"correlationId":"corr-42"`, `"providerId":"provider-a"`, `"durationMs"`} {
		if !strings.Contains(line, want) {
			t.Errorf("access log lacks %s: %s", want, line)
		}
	}
	for _, leak := range []string{"12345.67", "secret-token"} {
		if strings.Contains(line, leak) {
			t.Errorf("access log leaks %q: %s", leak, line)
		}
	}
}
