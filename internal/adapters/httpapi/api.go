package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
)

// Small consumer-side interfaces let handler tests use fakes and
// integration tests the real use cases.
type WagerSubmitter interface {
	Execute(ctx context.Context, cmd app.SubmitCommand) (app.SubmitResult, error)
}

type WagerReader interface {
	Transaction(ctx context.Context, id uuid.UUID) (*wager.Transaction, error)
	TransactionByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error)
}

type API struct {
	Auth Authenticator
	Log  *slog.Logger
	// 0 disables the timeout (tests).
	RequestTimeout time.Duration
	Submit         WagerSubmitter
	Reader         WagerReader
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.routes(mux)
	return a.wrap(mux)
}

func (a *API) wrap(h http.Handler) http.Handler {
	if a.RequestTimeout > 0 {
		h = withTimeout(a.RequestTimeout, h)
	}
	return withCorrelation(withAccessLog(a.Log, h))
}

func (a *API) routes(mux *http.ServeMux) {
	provider := func(h http.HandlerFunc) http.Handler {
		return require(a.Auth, a.Log, RoleWagerSubmit, h)
	}
	reader := func(h http.HandlerFunc) http.Handler {
		// Providers read their own transactions; the internal client reads all.
		return requireAny(a.Auth, a.Log, []string{RoleWagerRead, RoleWalletAdmin}, h)
	}
	mux.Handle("POST /wagering/transactions", provider(a.submit))
	mux.Handle("GET /wagering/transactions/{transactionId}", reader(a.getTransaction))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", reader(a.getByExternalID))
}
