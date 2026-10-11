package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
)

// Realm roles carried by access tokens.
const (
	RoleWagerSubmit = "wager:submit"
	RoleWagerRead   = "wager:read"
	RoleWalletAdmin = "wallet:admin"
)

var ErrUnauthenticated = errors.New("unauthenticated")

// ProviderID comes only from the verified token, never from the body or path
// (§2); it is empty for the internal client.
type Principal struct {
	ClientID   string
	ProviderID string
	Roles      []string
}

func (p Principal) Has(role string) bool { return slices.Contains(p.Roles, role) }

// Authenticator turns a request into a Principal; handlers do not depend on
// how tokens are verified.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// DenyAll refuses every request. The production graph uses it until the JWT
// verifier exists: business endpoints are never reachable without
// authentication.
type DenyAll struct{}

func (DenyAll) Authenticate(*http.Request) (Principal, error) { return Principal{}, ErrUnauthenticated }

const principalKey ctxKey = iota + 100

func principalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// require authenticates per route, so health and metrics stay public by
// construction and each business route states its policy where it is
// registered. Every authentication failure is a 401; the reason goes to the
// debug log, never to the client.
func require(a Authenticator, log *slog.Logger, role string, next http.Handler) http.Handler {
	return requireAny(a, log, []string{role}, next)
}

func requireAny(a Authenticator, log *slog.Logger, roles []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authenticate(r)
		if err != nil {
			log.DebugContext(r.Context(), "authentication failed", "err", err)
			w.Header().Set("WWW-Authenticate", `Bearer realm="wallet"`)
			writeProblem(w, r, http.StatusUnauthorized, codeUnauthenticated, "valid bearer token required")
			return
		}
		if info := requestInfoFrom(r.Context()); info != nil {
			info.clientID, info.providerID = p.ClientID, p.ProviderID
		}
		if !slices.ContainsFunc(roles, p.Has) {
			writeProblem(w, r, http.StatusForbidden, codeForbidden, "missing role")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}
