// Package logging provides the process-wide structured JSON logger.
package logging

import (
	"io"
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
)

// Module provides *slog.Logger and routes Fx's own events through it, so every
// line the process writes is JSON.
var Module = fx.Module("logging",
	fx.Provide(func(cfg config.Config) *slog.Logger {
		return New(os.Stdout, cfg.LogLevel, cfg.InstanceID)
	}),
)

// FxLogger makes Fx log through slog. Routine graph events (provide, invoke,
// hook timings) go to debug; Fx errors stay at error.
var FxLogger = fx.WithLogger(func(l *slog.Logger) fxevent.Logger {
	fl := &fxevent.SlogLogger{Logger: l.With("component", "fx")}
	fl.UseLogLevel(slog.LevelDebug)
	fl.UseErrorLevel(slog.LevelError)
	return fl
})

// New builds a JSON logger that tags every record with the service and the
// instance, which is what lets logs from three replicas be told apart.
func New(w io.Writer, level slog.Level, instanceID string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h).With("service", "wallet", "instanceId", instanceID)
}
