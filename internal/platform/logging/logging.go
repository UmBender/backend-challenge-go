// Package logging provides the process-wide structured JSON logger.
package logging

import (
	"io"
	"log/slog"
)

// New builds a JSON logger that tags every record with the service and the
// instance, which is what lets logs from three replicas be told apart.
func New(w io.Writer, level slog.Level, instanceID string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(h).With("service", "wallet", "instanceId", instanceID)
}
