package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func minimal() map[string]string {
	return map[string]string{
		"INSTANCE_ID":  "test-1",
		"DATABASE_URL": "postgres://wallet_app:secret@localhost:5432/wallet?sslmode=disable",
		"AWS_REGION":   "us-east-1",
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(env(minimal()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr = %q, want :8080", cfg.HTTP.Addr)
	}
	if cfg.StopTimeout != 25*time.Second {
		t.Errorf("StopTimeout = %v, want 25s", cfg.StopTimeout)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.SQS.WagerQueueName != "wager-transactions.fifo" {
		t.Errorf("WagerQueueName = %q", cfg.SQS.WagerQueueName)
	}
	if cfg.Database.MaxConns != 10 {
		t.Errorf("MaxConns = %d, want 10", cfg.Database.MaxConns)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	values := minimal()
	values["LOG_LEVEL"] = "debug"
	values["HTTP_ADDR"] = ":9999"
	values["DB_MAX_CONNS"] = "4"
	values["SHUTDOWN_TIMEOUT"] = "40s"
	values["AWS_ENDPOINT_URL"] = "http://localstack:4566"

	cfg, err := Load(env(values))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != slog.LevelDebug || cfg.HTTP.Addr != ":9999" || cfg.Database.MaxConns != 4 ||
		cfg.StopTimeout != 40*time.Second || cfg.AWS.EndpointURL != "http://localstack:4566" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		change  func(map[string]string)
		wantErr string
	}{
		{"missing database url", func(v map[string]string) { delete(v, "DATABASE_URL") }, "DATABASE_URL is required"},
		{"missing region", func(v map[string]string) { v["AWS_REGION"] = "  " }, "AWS_REGION is required"},
		{"non postgres url", func(v map[string]string) { v["DATABASE_URL"] = "mysql://x" }, "DATABASE_URL must be a postgres:// URL"},
		{"bad duration", func(v map[string]string) { v["SHUTDOWN_TIMEOUT"] = "soon" }, "SHUTDOWN_TIMEOUT must be a positive duration"},
		{"negative duration", func(v map[string]string) { v["HTTP_IDLE_TIMEOUT"] = "-1s" }, "HTTP_IDLE_TIMEOUT must be a positive duration"},
		{"bad int", func(v map[string]string) { v["DB_MAX_CONNS"] = "many" }, "DB_MAX_CONNS must be an integer"},
		{"zero conns", func(v map[string]string) { v["DB_MAX_CONNS"] = "0" }, "DB_MAX_CONNS must be greater than zero"},
		{"bad level", func(v map[string]string) { v["LOG_LEVEL"] = "loud" }, "LOG_LEVEL must be one of"},
		{"standard queue", func(v map[string]string) { v["SQS_DLQ_NAME"] = "dlq" }, "SQS_DLQ_NAME must name a FIFO queue"},
		{"relative endpoint", func(v map[string]string) { v["AWS_ENDPOINT_URL"] = "localstack:4566" }, "AWS_ENDPOINT_URL must be an absolute URL"},
		{
			"http drain longer than stop budget",
			func(v map[string]string) { v["HTTP_SHUTDOWN_TIMEOUT"] = "30s"; v["SHUTDOWN_TIMEOUT"] = "20s" },
			"HTTP_SHUTDOWN_TIMEOUT must be shorter than SHUTDOWN_TIMEOUT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := minimal()
			tt.change(values)
			_, err := Load(env(values))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{"INSTANCE_ID": "x", "DB_MAX_CONNS": "nope"}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"DATABASE_URL is required", "AWS_REGION is required", "DB_MAX_CONNS must be an integer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadNeverLeaksTheDatabasePassword(t *testing.T) {
	values := minimal()
	values["DATABASE_URL"] = "mysql://user:hunter2@host/db"
	_, err := Load(env(values))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error must exist and must not contain the password: %v", err)
	}
}

func TestLoadPendingPolicy(t *testing.T) {
	cfg, err := Load(env(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	want := Pending{TTL: 15 * time.Minute, MaxAttempts: 10, BackoffBase: time.Second, BackoffCap: 2 * time.Minute}
	if cfg.Pending != want {
		t.Fatalf("defaults = %+v, want %+v", cfg.Pending, want)
	}
	values := minimal()
	values["PENDING_REFERENCE_TTL"] = "2s"
	values["PENDING_MAX_ATTEMPTS"] = "3"
	values["PENDING_BACKOFF_BASE"] = "100ms"
	values["PENDING_BACKOFF_CAP"] = "1s"
	cfg, err = Load(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pending != (Pending{TTL: 2 * time.Second, MaxAttempts: 3, BackoffBase: 100 * time.Millisecond, BackoffCap: time.Second}) {
		t.Fatalf("overrides = %+v", cfg.Pending)
	}
}

func TestLoadRejectsInvalidPendingPolicy(t *testing.T) {
	for name, tt := range map[string]struct {
		change  func(map[string]string)
		wantErr string
	}{
		"zero attempts": {func(v map[string]string) { v["PENDING_MAX_ATTEMPTS"] = "0" }, "PENDING_MAX_ATTEMPTS must be at least 1"},
		"base over cap": {func(v map[string]string) { v["PENDING_BACKOFF_BASE"] = "5m" }, "PENDING_BACKOFF_BASE must not exceed PENDING_BACKOFF_CAP"},
	} {
		values := minimal()
		tt.change(values)
		if _, err := Load(env(values)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v, want %q", name, err, tt.wantErr)
		}
	}
}

func TestLoadRequestTimeout(t *testing.T) {
	cfg, err := Load(env(minimal()))
	if err != nil || cfg.HTTP.RequestTimeout != 10*time.Second {
		t.Fatalf("default request timeout = %v, %v; want 10s", cfg.HTTP.RequestTimeout, err)
	}
	values := minimal()
	values["HTTP_REQUEST_TIMEOUT"] = "40s"
	if _, err := Load(env(values)); err == nil || !strings.Contains(err.Error(), "HTTP_REQUEST_TIMEOUT must be shorter than HTTP_WRITE_TIMEOUT") {
		t.Fatalf("err = %v", err)
	}
}
