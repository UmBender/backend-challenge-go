//go:build integration

package bootstrap

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

// TestAppStartsServesAndStops runs the real graph against real PostgreSQL
// (the Compose service) and checks the lifecycle contract of T0:
// start validates dependencies, readiness is green, stop drains HTTP, closes
// the pool and releases the port.
//
// Needs the infra running (`make up`) and the variables from .env.example.
func TestAppStartsServesAndStops(t *testing.T) {
	requireEnv(t, "DATABASE_URL", "AWS_REGION")
	addr := freeAddr(t)
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("INSTANCE_ID", "integration-test")

	var pool *pgxpool.Pool
	app := fxtest.New(t, Modules(), fx.NopLogger, fx.Populate(&pool))
	app.RequireStart()

	base := "http://" + addr
	assertStatus(t, base+"/health/live", http.StatusOK)
	var ready struct {
		Ready  bool              `json:"ready"`
		Checks map[string]string `json:"checks"`
	}
	getJSON(t, base+"/health/ready", http.StatusOK, &ready)
	if !ready.Ready || ready.Checks["postgres"] != "ok" {
		t.Fatalf("readiness = %+v", ready)
	}

	app.RequireStop()

	if err := pool.Ping(context.Background()); err == nil {
		t.Fatal("postgres pool still usable after stop: it was not closed")
	}
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("HTTP port still accepting connections after stop")
	}
}

func requireEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if os.Getenv(k) == "" {
			t.Fatalf("%s is not set; export the variables from .env.example", k)
		}
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

func assertStatus(t *testing.T, url string, want int) {
	t.Helper()
	getJSON(t, url, want, nil)
}

func getJSON(t *testing.T, url string, want int, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d", url, resp.StatusCode, want)
	}
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
}
