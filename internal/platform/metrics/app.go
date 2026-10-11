package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// AppMetrics implements app.Metrics; the app layer only sees the port.
type AppMetrics struct {
	divergences prometheus.Counter
}

func NewAppMetrics(reg prometheus.Registerer) (*AppMetrics, error) {
	m := &AppMetrics{
		divergences: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wallet_reconciliation_divergence_total",
			Help: "Reconciliations whose stored balance differed from the ledger.",
		}),
	}
	if err := reg.Register(m.divergences); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *AppMetrics) ReconciliationDivergence() { m.divergences.Inc() }
