// Package httpapi is the HTTP adapter: routing, handlers and the server
// lifecycle. Business endpoints arrive in T5; T0 serves health and metrics.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/UmBender/backend-challenge-go/internal/platform/health"
)

func NewRouter(readiness *health.Readiness, gatherer prometheus.Gatherer, reg prometheus.Registerer, log *slog.Logger) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", live)
	mux.Handle("GET /health/ready", ready(readiness, log))
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))
	return mux, nil
}

func live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func ready(readiness *health.Readiness, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, failures := readiness.Evaluate(r.Context())
		for name, err := range failures {
			log.Warn("readiness check failed", "check", name, "err", err)
		}
		code := http.StatusOK
		if !status.Ready {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, status)
	})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body) // the status line is already sent; nothing useful to do on error
}
