// Package bootstrap is the composition root: the only place that lists every
// Fx module. cmd/wallet runs it; tests validate and start the same graph.
package bootstrap

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
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
		newOutboxRelay,
	),
	fx.Invoke(runOutboxRelay),
)

// Conservative fixed values: a lease far above one SQS send, small batches,
// and a retry cap that keeps retrying forever without hammering the broker.
func newOutboxRelay(store app.OutboxStore, sender app.EventSender, clock app.Clock, m app.OutboxMetrics,
	log *slog.Logger, cfg config.Config) *app.OutboxRelay {
	return app.NewOutboxRelay(store, sender, clock, m, log, app.OutboxRelayConfig{
		// Unique per process, so a restarted replica never settles rows
		// leased by its previous incarnation.
		Owner:        cfg.InstanceID + "-" + uuid.NewString()[:8],
		BatchSize:    50,
		Lease:        30 * time.Second,
		PollInterval: 500 * time.Millisecond,
		BackoffBase:  time.Second,
		BackoffCap:   time.Minute,
	})
}

// runOutboxRelay starts after the pool and the queues (its dependencies) and
// stops before them. Stopping cancels the loop; an event sent but not yet
// marked is sent again later with the same eventId.
func runOutboxRelay(lc fx.Lifecycle, relay *app.OutboxRelay) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				defer close(done)
				relay.Run(ctx)
			}()
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return stopCtx.Err()
			}
		},
	})
}

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
