package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/money"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

var (
	providerA = Principal{ClientID: "provider-a", ProviderID: "provider-a", Roles: []string{RoleWagerSubmit, RoleWagerRead}}
	providerB = Principal{ClientID: "provider-b", ProviderID: "provider-b", Roles: []string{RoleWagerSubmit, RoleWagerRead}}
	internal  = Principal{ClientID: "wallet-internal", Roles: []string{RoleWalletAdmin}}
)

type fakeSubmitter struct {
	res app.SubmitResult
	err error
	got app.SubmitCommand
}

func (f *fakeSubmitter) Execute(_ context.Context, cmd app.SubmitCommand) (app.SubmitResult, error) {
	f.got = cmd
	return f.res, f.err
}

type fakeReader struct{ txs []*wager.Transaction }

func (f *fakeReader) Transaction(_ context.Context, id uuid.UUID) (*wager.Transaction, error) {
	for _, t := range f.txs {
		if t.ID() == id {
			return t, nil
		}
	}
	return nil, app.ErrTransactionNotFound
}

func (f *fakeReader) TransactionByExternalID(_ context.Context, provider, ext string) (*wager.Transaction, error) {
	for _, t := range f.txs {
		if t.ProviderID() == provider && t.ExternalTransactionID() == ext {
			return t, nil
		}
	}
	return nil, app.ErrTransactionNotFound
}

// stored builds a transaction as the database would return it.
func stored(t *testing.T, provider, ext string, status wager.Status, code wager.FailureCode) *wager.Transaction {
	t.Helper()
	m, _ := money.Parse("25.00", "BRL")
	h := sha256.Sum256([]byte(ext))
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	s := wager.Snapshot{
		ID: uuid.New(), Origin: wager.OriginExternal, Kind: wager.KindBet, Status: status,
		WalletID: uuid.New(), PlayerID: uuid.New(), Money: m, ProviderID: provider, ExternalTransactionID: ext,
		IdempotencyKey: provider + ":" + ext, PayloadHash: h[:], RoundID: "r", GameID: "g",
		FailureCode: code, CreatedAt: now, UpdatedAt: now,
	}
	switch status {
	case wager.StatusProcessed:
		s.ResultBalance, _ = money.Parse("975.00", "BRL")
		s.ResultWalletVersion, s.ProcessedAt = 2, now
	case wager.StatusRejected, wager.StatusFailed:
		s.ProcessedAt = now
	case wager.StatusPendingReference:
		s.Kind, s.ReferenceExternalTransactionID = wager.KindRefund, "bet-1"
		s.Attempts, s.NextAttemptAt, s.ExpiresAt = 1, now.Add(time.Second), now.Add(15*time.Minute)
	}
	tx, err := wager.Rehydrate(s)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func serve(t *testing.T, api *API, p Principal, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	api.Auth = staticAuth{p: p}
	if api.Log == nil {
		api.Log = discard
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, r)
	return rec
}

func TestSubmitResponses(t *testing.T) {
	for name, tt := range map[string]struct {
		res      app.SubmitResult
		status   int
		want     map[string]any
		location bool
	}{
		"processed": {app.SubmitResult{Transaction: stored(t, "provider-a", "t1", wager.StatusProcessed, "")}, 201,
			map[string]any{"status": "PROCESSED", "idempotentReplay": false, "balance": map[string]any{"amount": "975.00", "currency": "BRL"}}, false},
		"replay": {app.SubmitResult{Transaction: stored(t, "provider-a", "t1", wager.StatusProcessed, ""), Replay: true}, 200,
			map[string]any{"status": "PROCESSED", "idempotentReplay": true}, false},
		"rejected": {app.SubmitResult{Transaction: stored(t, "provider-a", "t1", wager.StatusRejected, wager.CodeInsufficientFunds)}, 422,
			map[string]any{"status": "REJECTED", "failureCode": "INSUFFICIENT_FUNDS", "idempotentReplay": false}, false},
		"rejected replay": {app.SubmitResult{Transaction: stored(t, "provider-a", "t1", wager.StatusRejected, wager.CodeInsufficientFunds), Replay: true}, 422,
			map[string]any{"status": "REJECTED", "idempotentReplay": true}, false},
		"pending": {app.SubmitResult{Transaction: stored(t, "provider-a", "t1", wager.StatusPendingReference, "")}, 202,
			map[string]any{"status": "PENDING_REFERENCE", "idempotentReplay": false}, true},
	} {
		sub := &fakeSubmitter{res: tt.res}
		rec := serve(t, &API{Submit: sub, Reader: &fakeReader{}}, providerA, http.MethodPost, "/wagering/transactions",
			validSubmit, "Idempotency-Key", "provider-a:transaction-123")
		if rec.Code != tt.status {
			t.Errorf("%s: status %d, want %d: %s", name, rec.Code, tt.status, rec.Body)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["transactionId"] != tt.res.Transaction.ID().String() {
			t.Errorf("%s: transactionId = %v", name, body["transactionId"])
		}
		for k, v := range tt.want {
			got, _ := json.Marshal(body[k])
			exp, _ := json.Marshal(v)
			if string(got) != string(exp) {
				t.Errorf("%s: %s = %s, want %s", name, k, got, exp)
			}
		}
		if tt.status != 201 && tt.status != 200 {
			if _, has := body["balance"]; has {
				t.Errorf("%s: only processed operations return a balance", name)
			}
		}
		if loc := rec.Header().Get("Location"); tt.location != (loc == "/wagering/transactions/"+tt.res.Transaction.ID().String()) {
			t.Errorf("%s: Location = %q", name, loc)
		}
	}
}

func TestSubmitPassesKeyAndCorrelation(t *testing.T) {
	sub := &fakeSubmitter{res: app.SubmitResult{Transaction: stored(t, "provider-a", "t1", wager.StatusProcessed, "")}}
	serve(t, &API{Submit: sub, Reader: &fakeReader{}}, providerA, http.MethodPost, "/wagering/transactions", validSubmit,
		"Idempotency-Key", "provider-a:transaction-123", "X-Correlation-Id", "corr-1")
	if sub.got.IdempotencyKey != "provider-a:transaction-123" || sub.got.CorrelationID != "corr-1" || sub.got.CausationID == "" {
		t.Fatalf("command = %+v", sub.got)
	}
	// Without X-Correlation-Id, the idempotency key correlates the events.
	serve(t, &API{Submit: sub, Reader: &fakeReader{}}, providerA, http.MethodPost, "/wagering/transactions", validSubmit,
		"Idempotency-Key", "provider-a:transaction-123")
	if sub.got.CorrelationID != "provider-a:transaction-123" {
		t.Fatalf("correlation = %q, want the idempotency key", sub.got.CorrelationID)
	}
}

// TestSubmitProviderComesFromTheToken (§2): a provider cannot submit in
// another provider's name; nothing reaches the use case.
func TestSubmitProviderComesFromTheToken(t *testing.T) {
	sub := &fakeSubmitter{}
	rec := serve(t, &API{Submit: sub, Reader: &fakeReader{}}, providerB, http.MethodPost, "/wagering/transactions",
		validSubmit, "Idempotency-Key", "k")
	if rec.Code != 403 || decodeProblem(t, rec).Code != "PROVIDER_MISMATCH" {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if sub.got.IdempotencyKey != "" {
		t.Fatal("the use case was called")
	}
	// The internal client has no provider identity and cannot submit (Q15).
	if rec := serve(t, &API{Submit: sub, Reader: &fakeReader{}}, internal, http.MethodPost, "/wagering/transactions",
		validSubmit, "Idempotency-Key", "k"); rec.Code != 403 {
		t.Fatalf("internal client submit: %d", rec.Code)
	}
}

func TestSubmitMissingKeyReachesValidation(t *testing.T) {
	sub := &fakeSubmitter{err: &wager.InvalidRequestError{Code: wager.CodeMissingIdempotencyKey, Reason: "x"}}
	rec := serve(t, &API{Submit: sub, Reader: &fakeReader{}}, providerA, http.MethodPost, "/wagering/transactions", validSubmit)
	if rec.Code != 400 || decodeProblem(t, rec).Code != "MISSING_IDEMPOTENCY_KEY" {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
}

// TestTransactionReadsAreScopedToTheProvider: A sees its own, gets 404 for
// B's (existence is not revealed), the internal client sees all.
func TestTransactionReadsAreScopedToTheProvider(t *testing.T) {
	mine := stored(t, "provider-a", "t1", wager.StatusPendingReference, "")
	theirs := stored(t, "provider-b", "t2", wager.StatusProcessed, "")
	api := &API{Submit: &fakeSubmitter{}, Reader: &fakeReader{txs: []*wager.Transaction{mine, theirs}}}

	rec := serve(t, api, providerA, http.MethodGet, "/wagering/transactions/"+mine.ID().String(), "")
	if rec.Code != 200 {
		t.Fatalf("own transaction: %d", rec.Code)
	}
	var view map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view["status"] != "PENDING_REFERENCE" || view["providerId"] != "provider-a" || view["referenceExternalTransactionId"] != "bet-1" ||
		view["externalTransactionId"] != "t1" || view["expiresAt"] == nil {
		t.Fatalf("view = %v", view)
	}
	if rec := serve(t, api, providerA, http.MethodGet, "/wagering/transactions/"+theirs.ID().String(), ""); rec.Code != 404 ||
		decodeProblem(t, rec).Code != "TRANSACTION_NOT_FOUND" {
		t.Fatalf("other provider's transaction: %d", rec.Code)
	}
	if rec := serve(t, api, internal, http.MethodGet, "/wagering/transactions/"+theirs.ID().String(), ""); rec.Code != 200 {
		t.Fatalf("internal read: %d", rec.Code)
	}
	if rec := serve(t, api, providerA, http.MethodGet, "/wagering/transactions/not-a-uuid", ""); rec.Code != 400 {
		t.Fatalf("bad id: %d", rec.Code)
	}

	if rec := serve(t, api, providerA, http.MethodGet, "/providers/provider-a/wagering/transactions/t1", ""); rec.Code != 200 {
		t.Fatalf("own by external id: %d", rec.Code)
	}
	if rec := serve(t, api, providerA, http.MethodGet, "/providers/provider-b/wagering/transactions/t2", ""); rec.Code != 403 ||
		decodeProblem(t, rec).Code != "PROVIDER_MISMATCH" {
		t.Fatalf("other provider's namespace: %d", rec.Code)
	}
	if rec := serve(t, api, providerA, http.MethodGet, "/providers/provider-a/wagering/transactions/t2", ""); rec.Code != 404 {
		t.Fatalf("unknown external id: %d", rec.Code)
	}
	if rec := serve(t, api, internal, http.MethodGet, "/providers/provider-b/wagering/transactions/t2", ""); rec.Code != 200 {
		t.Fatalf("internal by external id: %d", rec.Code)
	}
}

func TestWageringRoutesRequireRoles(t *testing.T) {
	readOnly := Principal{ClientID: "x", ProviderID: "provider-a", Roles: []string{RoleWagerRead}}
	api := &API{Submit: &fakeSubmitter{}, Reader: &fakeReader{}}
	if rec := serve(t, api, readOnly, http.MethodPost, "/wagering/transactions", validSubmit, "Idempotency-Key", "k"); rec.Code != 403 {
		t.Fatalf("submit without wager:submit: %d", rec.Code)
	}
	noRoles := Principal{ClientID: "x", ProviderID: "provider-a"}
	if rec := serve(t, api, noRoles, http.MethodGet, "/wagering/transactions/"+uuid.NewString(), ""); rec.Code != 403 {
		t.Fatalf("read without wager:read: %d", rec.Code)
	}
}
