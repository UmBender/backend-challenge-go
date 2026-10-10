package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/UmBender/backend-challenge-go/internal/platform/config"
	"github.com/UmBender/backend-challenge-go/internal/platform/health"
)

// noShutdown satisfies fx.Shutdowner; these tests never expect Serve to die.
type noShutdown struct{}

func (noShutdown) Shutdown(...fx.ShutdownOption) error { return nil }

func testHTTPConfig(addr string) config.HTTP {
	return config.HTTP{
		Addr:              addr,
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       time.Second,
		WriteTimeout:      time.Second,
		IdleTimeout:       time.Second,
		ShutdownTimeout:   time.Second,
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// The lifecycle contract without any dependency: start serves, stop marks the
// instance not ready first, drains, and releases the port.
func TestServerServesBetweenStartAndStop(t *testing.T) {
	addr := freeAddr(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	readiness := health.New(nil, time.Second)
	handler, _, _ := newTestRouter(t)
	lc := fxtest.NewLifecycle(t)
	NewServer(lc, noShutdown{}, testHTTPConfig(addr), handler, readiness, log)

	lc.RequireStart()
	resp, err := http.Get("http://" + addr + "/health/live")
	if err != nil {
		t.Fatalf("GET after start: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live = %d", resp.StatusCode)
	}

	lc.RequireStop()
	if status, _ := readiness.Evaluate(context.Background()); status.Ready {
		t.Fatal("still ready after stop: readiness was not marked shutting down")
	}
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("port still accepting connections after stop")
	}
}

// Binding happens synchronously in OnStart, so a taken port fails startup
// instead of being reported after Fx has already declared the app started.
func TestServerStartFailsWhenPortIsTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	lc := fxtest.NewLifecycle(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	NewServer(lc, noShutdown{}, testHTTPConfig(ln.Addr().String()), http.NotFoundHandler(), health.New(nil, time.Second), log)
	if err := lc.Start(context.Background()); err == nil {
		_ = lc.Stop(context.Background())
		t.Fatal("start succeeded on a port already in use")
	}
}
