package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeCheck struct {
	name  string
	err   error
	delay time.Duration
}

func (f fakeCheck) Name() string { return f.name }

func (f fakeCheck) Check(ctx context.Context) error {
	select {
	case <-time.After(f.delay):
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestEvaluateAllHealthy(t *testing.T) {
	r := New([]Check{fakeCheck{name: "postgres"}, fakeCheck{name: "sqs"}}, time.Second)
	status, failures := r.Evaluate(context.Background())
	if !status.Ready || len(failures) != 0 {
		t.Fatalf("status = %+v, failures = %v", status, failures)
	}
	if status.Checks["postgres"] != "ok" || status.Checks["sqs"] != "ok" {
		t.Fatalf("checks = %v", status.Checks)
	}
}

func TestEvaluateOneFailingDependency(t *testing.T) {
	boom := errors.New("connection refused to 10.0.0.5")
	r := New([]Check{fakeCheck{name: "postgres", err: boom}, fakeCheck{name: "sqs"}}, time.Second)
	status, failures := r.Evaluate(context.Background())
	if status.Ready {
		t.Fatal("must not be ready")
	}
	if status.Checks["postgres"] != "unavailable" || status.Checks["sqs"] != "ok" {
		t.Fatalf("checks = %v", status.Checks)
	}
	if !errors.Is(failures["postgres"], boom) {
		t.Fatalf("failure detail lost: %v", failures)
	}
}

func TestEvaluateBoundsSlowChecks(t *testing.T) {
	r := New([]Check{fakeCheck{name: "slow", delay: time.Minute}}, 50*time.Millisecond)
	start := time.Now()
	status, failures := r.Evaluate(context.Background())
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("evaluation took %v, timeout not applied", elapsed)
	}
	if status.Ready || !errors.Is(failures["slow"], context.DeadlineExceeded) {
		t.Fatalf("status = %+v, failures = %v", status, failures)
	}
}

func TestEvaluateRunsChecksConcurrently(t *testing.T) {
	checks := []Check{
		fakeCheck{name: "a", delay: 200 * time.Millisecond},
		fakeCheck{name: "b", delay: 200 * time.Millisecond},
		fakeCheck{name: "c", delay: 200 * time.Millisecond},
	}
	start := time.Now()
	New(checks, time.Second).Evaluate(context.Background())
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("checks ran serially: %v", elapsed)
	}
}
