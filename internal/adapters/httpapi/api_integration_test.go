//go:build integration

package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres"
	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres/pgtest"
	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/platform/health"
)

// headerAuth picks a principal by the X-Test-Client header. Test only: the
// production graph uses JWTAuthenticator.
type headerAuth map[string]Principal

func (h headerAuth) Authenticate(r *http.Request) (Principal, error) {
	p, ok := h[r.Header.Get("X-Test-Client")]
	if !ok {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

type server struct {
	t   *testing.T
	url string
}

// newServer runs the real router and use cases against a fresh database.
func newServer(t *testing.T) *server {
	t.Helper()
	db := pgtest.New(t)
	tx := postgres.NewTxManager(db.Pool(t, pgtest.App))
	clock, ids := app.SystemClock{}, app.UUIDv7{}
	q := app.NewQueries(tx)
	api := &API{
		Auth:           headerAuth{"provider-a": providerA, "provider-b": providerB, "internal": internal},
		Log:            discard,
		RequestTimeout: 5 * time.Second,
		Submit:         app.NewSubmitWager(tx, clock, ids, app.PendingPolicy{TTL: time.Minute, MaxAttempts: 5, BackoffBase: time.Second, BackoffCap: time.Minute}),
		Reader:         q,
		Opener:         app.NewOpenWallet(tx, clock, ids),
		Wallets:        q,
		Reconciler:     app.NewReconcile(tx, app.NopMetrics{}, discard),
	}
	reg := prometheus.NewRegistry()
	h, err := NewRouter(health.New(nil, time.Second), reg, reg, discard, api)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &server{t: t, url: srv.URL}
}

func (s *server) do(client, method, path, body string, headers ...string) (int, map[string]any, http.Header) {
	s.t.Helper()
	req, _ := http.NewRequest(method, s.url+path, strings.NewReader(body))
	req.Header.Set("X-Test-Client", client)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, resp.Header
}

func (s *server) openWallet(player, amount string) string {
	s.t.Helper()
	code, body, _ := s.do("internal", http.MethodPost, "/wallets",
		fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, player, amount))
	if code != 201 {
		s.t.Fatalf("open wallet: %d %v", code, body)
	}
	return body["id"].(string)
}

func wagerBody(wallet, player, kind, amount, ext, ref string) string {
	b := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,"walletId":%q,`+
		`"roundId":"round-1","gameId":"game-1","kind":%q,"money":{"amount":%q,"currency":"BRL"}`, ext, player, wallet, kind, amount)
	if ref != "" {
		b += fmt.Sprintf(`,"referenceExternalTransactionId":%q`, ref)
	}
	return b + "}"
}

const player = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"

func TestHTTPFlowAgainstTheDatabase(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	wallet := s.openWallet(player, "100.00")

	// OPENING from outside is refused (§6.3).
	code, body, _ := s.do("provider-a", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "OPENING", "1.00", "o-1", ""), "Idempotency-Key", "ko")
	if code != 400 || body["code"] != "UNSUPPORTED_KIND" {
		t.Fatalf("OPENING: %d %v", code, body)
	}

	bet := wagerBody(wallet, player, "BET", "25.00", "bet-1", "")
	code, first, _ := s.do("provider-a", http.MethodPost, "/wagering/transactions", bet, "Idempotency-Key", "provider-a:bet-1")
	if code != 201 || first["idempotentReplay"] != false || first["balance"].(map[string]any)["amount"] != "75.00" {
		t.Fatalf("bet: %d %v", code, first)
	}
	s.do("provider-a", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "BET", "5.00", "bet-2", ""), "Idempotency-Key", "provider-a:bet-2")

	// Replay: 200, idempotentReplay, the original balance.
	code, replay, _ := s.do("provider-a", http.MethodPost, "/wagering/transactions", bet, "Idempotency-Key", "provider-a:bet-1")
	if code != 200 || replay["idempotentReplay"] != true || replay["transactionId"] != first["transactionId"] ||
		replay["balance"].(map[string]any)["amount"] != "75.00" {
		t.Fatalf("replay: %d %v", code, replay)
	}
	code, body, _ = s.do("provider-a", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "BET", "26.00", "bet-1", ""), "Idempotency-Key", "provider-a:bet-1")
	if code != 409 || body["code"] != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("key reuse: %d %v", code, body)
	}
	code, body, _ = s.do("provider-a", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "BET", "500.00", "bet-3", ""), "Idempotency-Key", "provider-a:bet-3")
	if code != 422 || body["failureCode"] != "INSUFFICIENT_FUNDS" {
		t.Fatalf("rejection: %d %v", code, body)
	}

	code, pending, hdr := s.do("provider-a", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "REFUND", "10.00", "refund-1", "bet-late"), "Idempotency-Key", "provider-a:refund-1")
	if code != 202 || pending["status"] != "PENDING_REFERENCE" {
		t.Fatalf("pending: %d %v", code, pending)
	}
	code, view, _ := s.do("provider-a", http.MethodGet, hdr.Get("Location"), "")
	if code != 200 || view["status"] != "PENDING_REFERENCE" {
		t.Fatalf("follow Location: %d %v", code, view)
	}
	// Provider B cannot see A's transaction, by id or by external id.
	if code, _, _ := s.do("provider-b", http.MethodGet, hdr.Get("Location"), ""); code != 404 {
		t.Fatalf("provider B read A's transaction: %d", code)
	}
	if code, _, _ := s.do("provider-b", http.MethodGet, "/providers/provider-a/wagering/transactions/refund-1", ""); code != 403 {
		t.Fatalf("provider B read A's namespace: %d", code)
	}

	code, rec, _ := s.do("internal", http.MethodPost, "/wallets/"+wallet+"/reconciliation", "")
	if code != 200 || rec["consistent"] != true || rec["storedBalance"].(map[string]any)["amount"] != "70.00" || rec["checkedEntries"] != float64(3) {
		t.Fatalf("reconciliation: %d %v", code, rec)
	}
}

// TestLedgerPaginationIsStableOverHTTP: following nextCursor while new entries
// arrive yields every entry once, in order.
func TestLedgerPaginationIsStableOverHTTP(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	wallet := s.openWallet(player, "100.00")
	for i := range 3 {
		s.do("provider-a", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "BET", "1.00", fmt.Sprintf("b%d", i), ""), "Idempotency-Key", fmt.Sprintf("k%d", i))
	}
	var versions []float64
	path := "/wallets/" + wallet + "/ledger?limit=2"
	for page := 0; ; page++ {
		code, body, _ := s.do("internal", http.MethodGet, path, "")
		if code != 200 {
			t.Fatalf("page %d: %d %v", page, code, body)
		}
		for _, it := range body["items"].([]any) {
			versions = append(versions, it.(map[string]any)["walletVersion"].(float64))
		}
		if page == 0 {
			s.do("provider-a", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "WIN", "1.00", "late", ""), "Idempotency-Key", "late")
		}
		next, ok := body["nextCursor"].(string)
		if !ok {
			break
		}
		path = "/wallets/" + wallet + "/ledger?limit=2&cursor=" + next
	}
	if fmt.Sprint(versions) != "[1 2 3 4 5]" {
		t.Fatalf("versions = %v", versions)
	}
}
