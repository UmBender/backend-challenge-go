package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"go.uber.org/fx"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
	"github.com/UmBender/backend-challenge-go/internal/platform/health"
)

// Module provides the router and the server, and forces the server to be
// built so its lifecycle hooks are registered.
var Module = fx.Module("httpapi",
	fx.Provide(
		NewRouter,
		NewServer,
		NewAPI,
		func() Authenticator { return DenyAll{} },
	),
	fx.Invoke(func(*http.Server) {}),
)

// NewServer configures the server and registers its lifecycle:
//
//   - OnStart binds the port synchronously, so "address already in use" fails
//     startup, then serves in a goroutine. If Serve dies unexpectedly the whole
//     app is shut down with exit code 1 rather than running without HTTP.
//   - OnStop marks readiness as failing, then drains in-flight requests for at
//     most HTTP_SHUTDOWN_TIMEOUT, and force-closes what is left.
func NewServer(
	lc fx.Lifecycle,
	shutdowner fx.Shutdowner,
	cfg config.HTTP,
	handler http.Handler,
	readiness *health.Readiness,
	log *slog.Logger,
) *http.Server {
	log = log.With("component", "http")
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return fmt.Errorf("http listen on %s: %w", srv.Addr, err)
			}
			log.Info("http server listening", "addr", ln.Addr().String())
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("http server failed", "err", err)
					_ = shutdowner.Shutdown(fx.ExitCode(1))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			readiness.MarkShuttingDown()
			log.Info("http server draining", "timeout", cfg.ShutdownTimeout.String())
			drainCtx, cancel := context.WithTimeout(ctx, cfg.ShutdownTimeout)
			defer cancel()
			if err := srv.Shutdown(drainCtx); err != nil {
				_ = srv.Close()
				return fmt.Errorf("http drain incomplete, connections force-closed: %w", err)
			}
			log.Info("http server stopped")
			return nil
		},
	})
	return srv
}
