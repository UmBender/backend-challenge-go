package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/domain/wager"
	"github.com/UmBender/backend-challenge-go/internal/domain/wallet"
	"github.com/UmBender/backend-challenge-go/internal/platform/config"
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
	Opener         WalletOpener
	Wallets        WalletReader
	Reconciler     Reconciler
}

type WalletOpener interface {
	Execute(ctx context.Context, cmd app.OpenWalletCommand) (*wallet.Wallet, error)
}

type WalletReader interface {
	Wallet(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	Ledger(ctx context.Context, walletID uuid.UUID, afterSeq int64, limit int) (app.LedgerPage, error)
}

type Reconciler interface {
	Execute(ctx context.Context, walletID uuid.UUID) (app.Reconciliation, error)
}

// Handler serves only the business routes (handler tests).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	a.Register(mux)
	return mux
}

// Register mounts every business route on mux, each wrapped on its own. The
// wrapping is per route, not around a sub-mux, so the server's mux sets the
// route pattern on the request that the access log and the metrics read.
func (a *API) Register(mux *http.ServeMux) {
	handle := func(pattern string, h http.Handler) { mux.Handle(pattern, a.wrap(h)) }
	provider := func(h http.HandlerFunc) http.Handler {
		return require(a.Auth, a.Log, RoleWagerSubmit, h)
	}
	reader := func(h http.HandlerFunc) http.Handler {
		// Providers read their own transactions; the internal client reads all.
		return requireAny(a.Auth, a.Log, []string{RoleWagerRead, RoleWalletAdmin}, h)
	}
	handle("POST /wagering/transactions", provider(a.submit))
	handle("GET /wagering/transactions/{transactionId}", reader(a.getTransaction))
	handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", reader(a.getByExternalID))

	// Wallet operations are restricted to the internal service (§2).
	admin := func(h http.HandlerFunc) http.Handler {
		return require(a.Auth, a.Log, RoleWalletAdmin, h)
	}
	handle("POST /wallets", admin(a.openWallet))
	handle("GET /wallets/{walletId}", admin(a.getWallet))
	handle("GET /wallets/{walletId}/ledger", admin(a.getLedger))
	handle("POST /wallets/{walletId}/reconciliation", admin(a.reconcile))
}

func (a *API) wrap(h http.Handler) http.Handler {
	if a.RequestTimeout > 0 {
		h = withTimeout(a.RequestTimeout, h)
	}
	return withCorrelation(withAccessLog(a.Log, h))
}

// NewAPI is the Fx constructor. Auth is DenyAll until the JWT verifier is
// provided in its place.
func NewAPI(cfg config.HTTP, log *slog.Logger, auth Authenticator, submit *app.SubmitWager, queries *app.Queries,
	opener *app.OpenWallet, reconciler *app.Reconcile) *API {
	return &API{
		Auth: auth, Log: log, RequestTimeout: cfg.RequestTimeout,
		Submit: submit, Reader: queries, Opener: opener, Wallets: queries, Reconciler: reconciler,
	}
}
