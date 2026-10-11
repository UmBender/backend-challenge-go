package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
)

// JWTAuthenticator validates Keycloak access tokens: signature against the
// cached JWKS, a fixed algorithm, issuer, audience and expiry. The service
// never issues tokens.
type JWTAuthenticator struct {
	verifier *oidc.IDTokenVerifier
}

func NewJWTAuthenticator(cfg config.Auth) *JWTAuthenticator {
	// Keys are fetched lazily and refreshed when an unknown key id appears.
	keys := oidc.NewRemoteKeySet(context.Background(), cfg.JWKSURL)
	return &JWTAuthenticator{verifier: oidc.NewVerifier(cfg.Issuer, keys, &oidc.Config{
		ClientID:             cfg.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
	})}
}

type accessClaims struct {
	AuthorizedParty string `json:"azp"`
	ProviderID      string `json:"provider_id"`
	RealmAccess     struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (a *JWTAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || raw == "" {
		return Principal{}, fmt.Errorf("%w: missing bearer token", ErrUnauthenticated)
	}
	token, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	var c accessClaims
	if err := token.Claims(&c); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if c.AuthorizedParty == "" {
		return Principal{}, fmt.Errorf("%w: %w", ErrUnauthenticated, errors.New("token without azp"))
	}
	return Principal{ClientID: c.AuthorizedParty, ProviderID: c.ProviderID, Roles: c.RealmAccess.Roles}, nil
}
