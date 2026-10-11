// Package bootstrap is the composition root: the only place that lists every
// Fx module. cmd/wallet runs it; tests validate and start the same graph.
package bootstrap

import (
	"go.uber.org/fx"

	"github.com/UmBender/backend-challenge-go/internal/adapters/httpapi"
	"github.com/UmBender/backend-challenge-go/internal/adapters/postgres"
	"github.com/UmBender/backend-challenge-go/internal/adapters/sqsclient"
	"github.com/UmBender/backend-challenge-go/internal/app"
	"github.com/UmBender/backend-challenge-go/internal/platform/config"
	"github.com/UmBender/backend-challenge-go/internal/platform/health"
	"github.com/UmBender/backend-challenge-go/internal/platform/logging"
	"github.com/UmBender/backend-challenge-go/internal/platform/metrics"
)

// Modules is the full application graph, without process-level options.
func Modules() fx.Option {
	return fx.Options(
		config.Module,
		logging.Module,
		metrics.Module,
		health.Module,
		postgres.Module,
		sqsclient.Module,
		httpapi.Module,
		appModule,
	)
}

// appModule lives here so the application layer stays free of Fx.
var appModule = fx.Module("app",
	fx.Provide(
		func() app.Clock { return app.SystemClock{} },
		func() app.IDs { return app.UUIDv7{} },
		func(p config.Pending) app.PendingPolicy {
			return app.PendingPolicy{TTL: p.TTL, MaxAttempts: p.MaxAttempts, BackoffBase: p.BackoffBase, BackoffCap: p.BackoffCap}
		},
		app.NewOpenWallet,
		app.NewSubmitWager,
		app.NewQueries,
		app.NewReconcile,
	),
)

// App is the production application: the graph plus logging and the start
// and stop budgets from configuration.
func App() *fx.App {
	cfg, err := config.FromEnv()
	timeouts := fx.Options()
	if err == nil {
		// On error, config.Module fails the same way inside Fx and reports it.
		timeouts = fx.Options(fx.StartTimeout(cfg.StartTimeout), fx.StopTimeout(cfg.StopTimeout))
	}
	return fx.New(Modules(), logging.FxLogger, timeouts)
}
