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

	// Wallet operations are restricted to the internal service (§2).
	admin := func(h http.HandlerFunc) http.Handler {
		return require(a.Auth, a.Log, RoleWalletAdmin, h)
	}
	mux.Handle("POST /wallets", admin(a.openWallet))
	mux.Handle("GET /wallets/{walletId}", admin(a.getWallet))
	mux.Handle("GET /wallets/{walletId}/ledger", admin(a.getLedger))
	mux.Handle("POST /wallets/{walletId}/reconciliation", admin(a.reconcile))
}
