// Package health implements liveness and readiness. Liveness never checks
// dependencies (a database blip must not restart every replica); readiness
// checks the dependencies the service cannot work without.
package health

import (
	"context"
	"sync"
	"time"

	"go.uber.org/fx"
)

// Check is one readiness dependency, such as PostgreSQL or SQS.
type Check interface {
	Name() string
	Check(ctx context.Context) error
}

// CheckGroup is the Fx value group that collects every readiness Check.
const CheckGroup = `group:"readiness"`

// AsCheck annotates a constructor so its result joins the readiness group.
func AsCheck(constructor any) any {
	return fx.Annotate(constructor, fx.As(new(Check)), fx.ResultTags(CheckGroup))
}

// Status is the outcome of one readiness evaluation.
type Status struct {
	Ready  bool              `json:"ready"`
	Reason string            `json:"reason,omitempty"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Readiness evaluates every registered Check concurrently, each under its own
// timeout.
type Readiness struct {
	checks  []Check
	timeout time.Duration
}

// ReadinessParams lets Fx inject the whole readiness group.
type ReadinessParams struct {
	fx.In
	Checks []Check `group:"readiness"`
}

// NewReadiness is the Fx constructor.
func NewReadiness(p ReadinessParams) *Readiness {
	return New(p.Checks, time.Second)
}

// New builds a Readiness with an explicit per-check timeout.
func New(checks []Check, timeout time.Duration) *Readiness {
	return &Readiness{checks: checks, timeout: timeout}
}

// Evaluate runs all checks and never takes longer than the per-check timeout.
// Error texts are not returned to callers: only "ok" or "unavailable", so the
// public endpoint does not leak hostnames or driver messages.
func (r *Readiness) Evaluate(ctx context.Context) (Status, map[string]error) {
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(r.checks))
	var wg sync.WaitGroup
	for _, c := range r.checks {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			results <- result{name: c.Name(), err: c.Check(cctx)}
		})
	}
	wg.Wait()
	close(results)

	status := Status{Ready: true, Checks: make(map[string]string, len(r.checks))}
	failures := make(map[string]error)
	for res := range results {
		if res.err != nil {
			status.Ready = false
			status.Checks[res.name] = "unavailable"
			failures[res.name] = res.err
		} else {
			status.Checks[res.name] = "ok"
		}
	}
	return status, failures
}
