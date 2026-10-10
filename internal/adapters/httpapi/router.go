// Package httpapi is the HTTP adapter: routing, handlers and the server
// lifecycle. Business endpoints arrive in T5; T0 serves health and metrics.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/UmBender/backend-challenge-go/internal/platform/health"
)

func NewRouter(readiness *health.Readiness, gatherer prometheus.Gatherer, reg prometheus.Registerer, log *slog.Logger) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", live)
	mux.Handle("GET /health/ready", ready(readiness, log))
	mux.Handle("GET /metrics", promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{}))

	m, err := newHTTPMetrics(reg)
	if err != nil {
		return nil, err
	}
	return m.instrument(mux), nil
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

// httpMetrics records request count and latency per route pattern. Labelling
// by the matched pattern (not the raw path) keeps cardinality bounded: ids in
// paths such as /wallets/{walletId} never become label values.
type httpMetrics struct {
	duration *prometheus.HistogramVec
}

func newHTTPMetrics(reg prometheus.Registerer) (*httpMetrics, error) {
	d := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by route pattern, method and status code.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method", "code"})
	if err := reg.Register(d); err != nil {
		return nil, err
	}
	return &httpMetrics{duration: d}, nil
}

func (m *httpMetrics) instrument(next *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := r.Pattern // set by ServeMux on the request it was given
		if route == "" {
			route = "unmatched"
		}
		m.duration.WithLabelValues(route, r.Method, strconv.Itoa(rec.code)).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
