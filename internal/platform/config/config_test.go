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
