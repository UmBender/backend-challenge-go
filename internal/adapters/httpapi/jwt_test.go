package httpapi

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
)

const (
	testIssuer   = "http://idp.test/realms/wagering"
	testAudience = "wallet-api"
)

// newTestIdP serves a JWKS with one RSA key and returns an authenticator
// trusting it, plus the key to sign tokens with.
func newTestIdP(t *testing.T) (*JWTAuthenticator, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return NewJWTAuthenticator(config.Auth{Issuer: testIssuer, JWKSURL: srv.URL, Audience: testAudience}), key
}

func sign(t *testing.T, alg jose.SignatureAlgorithm, key any, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func validClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": testIssuer, "aud": testAudience, "azp": "provider-a", "sub": "sa-provider-a",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		"provider_id":  "provider-a",
		"realm_access": map[string]any{"roles": []string{RoleWagerSubmit, RoleWagerRead}},
	}
}

func authRequest(header string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

func TestJWTAuthenticatorAcceptsAValidToken(t *testing.T) {
	auth, key := newTestIdP(t)
	p, err := auth.Authenticate(authRequest("Bearer " + sign(t, jose.RS256, key, validClaims())))
	if err != nil {
		t.Fatal(err)
	}
	if p.ClientID != "provider-a" || p.ProviderID != "provider-a" || !slices.Equal(p.Roles, []string{RoleWagerSubmit, RoleWagerRead}) {
		t.Fatalf("principal = %+v", p)
	}
}

func TestJWTAuthenticatorRejects(t *testing.T) {
	auth, key := newTestIdP(t)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	with := func(k string, v any) map[string]any { c := validClaims(); c[k] = v; return c }
	without := func(k string) map[string]any { c := validClaims(); delete(c, k); return c }

	for name, header := range map[string]string{
		"missing header":    "",
		"wrong scheme":      "Basic " + sign(t, jose.RS256, key, validClaims()),
		"empty token":       "Bearer ",
		"malformed":         "Bearer not.a.jwt",
		"other signing key": "Bearer " + sign(t, jose.RS256, otherKey, validClaims()),
		"HMAC algorithm":    "Bearer " + sign(t, jose.HS256, []byte("0123456789abcdef0123456789abcdef"), validClaims()),
		"expired":           "Bearer " + sign(t, jose.RS256, key, with("exp", time.Now().Add(-time.Minute).Unix())),
		"wrong audience":    "Bearer " + sign(t, jose.RS256, key, with("aud", "account")),
		"wrong issuer":      "Bearer " + sign(t, jose.RS256, key, with("iss", "http://evil.test/realms/wagering")),
		"no azp":            "Bearer " + sign(t, jose.RS256, key, without("azp")),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := auth.Authenticate(authRequest(header)); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}
