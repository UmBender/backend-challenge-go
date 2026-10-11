package metrics

import (
	"testing"

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
