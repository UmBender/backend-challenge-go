package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/UmBender/backend-challenge-go/internal/platform/health"
)

type stubCheck struct {
	name string
	err  error
}

func (s stubCheck) Name() string                { return s.name }
func (s stubCheck) Check(context.Context) error { return s.err }

func newTestRouter(t *testing.T, checks ...health.Check) (http.Handler, *health.Readiness, *prometheus.Registry) {
	t.Helper()
	readiness := health.New(checks, time.Second)
	reg := prometheus.NewRegistry()
	h, err := NewRouter(readiness, reg, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return h, readiness, reg
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestLiveIgnoresDependencies(t *testing.T) {
	h, _, _ := newTestRouter(t, stubCheck{name: "postgres", err: errors.New("down")})
	rec := get(t, h, "/health/live")
	if rec.Code != http.StatusOK {
		t.Fatalf("live = %d, want 200 even with a failing dependency", rec.Code)
	}
}

func TestReadyReflectsDependencies(t *testing.T) {
	h, _, _ := newTestRouter(t, stubCheck{name: "postgres"}, stubCheck{name: "sqs"})
	rec := get(t, h, "/health/ready")
	if rec.Code != http.StatusOK {
		t.Fatalf("ready = %d, want 200", rec.Code)
	}
	var body health.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.Ready {
		t.Fatalf("body = %s, err = %v", rec.Body, err)
	}
}

func TestReadyFailsAndHidesErrorDetails(t *testing.T) {
	h, _, _ := newTestRouter(t, stubCheck{name: "postgres", err: errors.New("dial tcp 10.1.2.3:5432: refused")})
	rec := get(t, h, "/health/ready")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.1.2.3") {
		t.Fatalf("readiness body leaks infrastructure details: %s", rec.Body)
	}
}

func TestReadyFailsDuringShutdown(t *testing.T) {
	h, readiness, _ := newTestRouter(t, stubCheck{name: "postgres"})
	readiness.MarkShuttingDown()
	if rec := get(t, h, "/health/ready"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready = %d, want 503 while shutting down", rec.Code)
	}
}

func TestUnknownMethodAndPath(t *testing.T) {
	h, _, _ := newTestRouter(t)
	if rec := get(t, h, "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404", rec.Code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health/live", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health/live = %d, want 405", rec.Code)
	}
}

func TestMetricsUseRoutePatternLabels(t *testing.T) {
	h, _, _ := newTestRouter(t)
	get(t, h, "/health/live")
	get(t, h, "/does-not-exist/123")
	body := get(t, h, "/metrics").Body.String()
	if !strings.Contains(body, `http_request_duration_seconds_count{code="200",method="GET",route="GET /health/live"} 1`) {
		t.Fatalf("missing labelled request metric:\n%s", body)
	}
	if strings.Contains(body, "/does-not-exist/123") {
		t.Fatal("raw paths must never become label values")
	}
	if !strings.Contains(body, `route="unmatched"`) {
		t.Fatal("unmatched requests should be grouped under one label")
	}
}
