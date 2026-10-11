//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
)

// keycloakToken obtains a real access token with client_credentials, using the
// local test clients provisioned by deploy/keycloak/realm-wagering.json.
func keycloakToken(t *testing.T, clientID string) string {
	t.Helper()
	resp, err := http.PostForm(os.Getenv("OIDC_ISSUER")+"/protocol/openid-connect/token", url.Values{
		"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientID + "-secret"},
	})
	if err != nil {
		t.Fatalf("token for %s (is Keycloak up?): %v", clientID, err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("token for %s: status %d, err %v", clientID, resp.StatusCode, err)
	}
	return body.AccessToken
}

func newKeycloakServer(t *testing.T) *server {
	t.Helper()
	for _, k := range []string{"OIDC_ISSUER", "OIDC_JWKS_URL", "OIDC_AUDIENCE"} {
		if os.Getenv(k) == "" {
			t.Fatalf("%s is not set; export the variables from .env.example", k)
		}
	}
	return newServerWith(t, NewJWTAuthenticator(config.Auth{
		Issuer: os.Getenv("OIDC_ISSUER"), JWKSURL: os.Getenv("OIDC_JWKS_URL"), Audience: os.Getenv("OIDC_AUDIENCE"),
	}))
}

func bearer(token string) []string { return []string{"Authorization", "Bearer " + token} }

func TestKeycloakCredentialsAreRequired(t *testing.T) {
	t.Parallel()
	s := newKeycloakServer(t)
	internalTok := keycloakToken(t, "wallet-internal")
	code, w, _ := s.do("", http.MethodPost, "/wallets",
		`{"playerId":"`+player+`","initialBalance":{"amount":"100.00","currency":"BRL"}}`, bearer(internalTok)...)
	if code != 201 {
		t.Fatalf("open wallet: %d %v", code, w)
	}
	wallet := w["id"].(string)
	bet := wagerBody(wallet, player, "BET", "10.00", "unauth-1", "")

	valid := keycloakToken(t, "provider-a")
	tampered := valid[:len(valid)-4] + "AAAA"
	shortLived := keycloakToken(t, "provider-a-short-lived")
	time.Sleep(2 * time.Second) // lifespan is 1 s
	for name, headers := range map[string][]string{
		"no token":           nil,
		"malformed token":    bearer("not.a.jwt"),
		"tampered signature": bearer(tampered),
		"expired token":      bearer(shortLived),
		"wrong audience":     bearer(keycloakToken(t, "other-service")),
	} {
		code, body, _ := s.do("", http.MethodPost, "/wagering/transactions", bet, append(headers, "Idempotency-Key", "unauth-1")...)
		if code != 401 || body["code"] != "UNAUTHENTICATED" {
			t.Errorf("%s: %d %v, want 401", name, code, body)
		}
	}
	// None of the refused requests reached the database.
	code, _, _ = s.do("", http.MethodGet, "/providers/provider-a/wagering/transactions/unauth-1", "", bearer(valid)...)
	if code != 404 {
		t.Fatalf("a refused request left a transaction behind: %d", code)
	}
}

func TestKeycloakIdentityDecidesAccess(t *testing.T) {
	t.Parallel()
	s := newKeycloakServer(t)
	a, b, internalTok := keycloakToken(t, "provider-a"), keycloakToken(t, "provider-b"), keycloakToken(t, "wallet-internal")

	// Wallet operations belong to the internal service only.
	open := `{"playerId":"` + player + `","initialBalance":{"amount":"100.00","currency":"BRL"}}`
	if code, _, _ := s.do("", http.MethodPost, "/wallets", open, bearer(a)...); code != 403 {
		t.Fatalf("provider opened a wallet: %d", code)
	}
	code, w, _ := s.do("", http.MethodPost, "/wallets", open, bearer(internalTok)...)
	if code != 201 {
		t.Fatalf("open wallet: %d %v", code, w)
	}
	wallet := w["id"].(string)
	if code, _, _ := s.do("", http.MethodGet, "/wallets/"+wallet, "", bearer(a)...); code != 403 {
		t.Fatalf("provider read a wallet: %d", code)
	}

	// Provider A operates as itself.
	code, mine, _ := s.do("", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "BET", "10.00", "bet-a", ""),
		append(bearer(a), "Idempotency-Key", "shared-key")...)
	if code != 201 {
		t.Fatalf("provider A bet: %d %v", code, mine)
	}

	// Provider B cannot send as A, nor see A's transaction or namespace.
	code, body, _ := s.do("", http.MethodPost, "/wagering/transactions", wagerBody(wallet, player, "BET", "10.00", "bet-b", ""),
		append(bearer(b), "Idempotency-Key", "b-1")...)
	if code != 403 || body["code"] != "PROVIDER_MISMATCH" {
		t.Fatalf("B sent as A: %d %v", code, body)
	}
	if code, _, _ := s.do("", http.MethodGet, "/wagering/transactions/"+mine["transactionId"].(string), "", bearer(b)...); code != 404 {
		t.Fatalf("B read A's transaction: %d", code)
	}
	if code, _, _ := s.do("", http.MethodGet, "/providers/provider-a/wagering/transactions/bet-a", "", bearer(b)...); code != 403 {
		t.Fatalf("B read A's namespace: %d", code)
	}

	// Reusing A's key from B's identity is a different, B-scoped request: it
	// never replays A's result.
	bBody := strings.ReplaceAll(wagerBody(wallet, player, "BET", "10.00", "bet-a", ""), `"providerId":"provider-a"`, `"providerId":"provider-b"`)
	code, theirs, _ := s.do("", http.MethodPost, "/wagering/transactions", bBody, append(bearer(b), "Idempotency-Key", "shared-key")...)
	if code != 201 || theirs["idempotentReplay"] != false || theirs["transactionId"] == mine["transactionId"] {
		t.Fatalf("B's request with A's key: %d %v", code, theirs)
	}

	// The internal client reads any transaction.
	if code, _, _ := s.do("", http.MethodGet, "/wagering/transactions/"+mine["transactionId"].(string), "", bearer(internalTok)...); code != 200 {
		t.Fatalf("internal read: %d", code)
	}
}
