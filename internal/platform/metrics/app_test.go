package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/UmBender/backend-challenge-go/internal/app"
)

func TestAppMetricsCountReconciliationDivergences(t *testing.T) {
	reg := NewRegistry()
	m, err := NewAppMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	var port app.Metrics = m
	port.ReconciliationDivergence()
	port.ReconciliationDivergence()
	if got := testutil.ToFloat64(m.divergences); got != 2 {
		t.Fatalf("wallet_reconciliation_divergence_total = %v, want 2", got)
	}
}

func TestAppMetricsCountWagerOutcomes(t *testing.T) {
	m, err := NewAppMetrics(NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	var port app.WagerMetrics = m
	port.WagerResult("http", "PROCESSED", false, time.Millisecond)
	port.WagerResult("http", "PROCESSED", true, time.Millisecond)
	port.WagerResult("sqs", "REJECTED", false, time.Millisecond)
	port.ConcurrencyConflict()
	m.SQSMessage("dlq")
	if got := testutil.ToFloat64(m.wagerResults.WithLabelValues("http", "PROCESSED")); got != 2 {
		t.Fatalf("http PROCESSED = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.wagerReplays.WithLabelValues("http")); got != 1 {
		t.Fatalf("duplicates = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.conflicts); got != 1 {
		t.Fatalf("conflicts = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.sqsMessages.WithLabelValues("dlq")); got != 1 {
		t.Fatalf("dlq = %v, want 1", got)
	}
}
