package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// AppMetrics implements app.Metrics; the app layer only sees the port.
type AppMetrics struct {
	divergences     prometheus.Counter
	outboxPublished prometheus.Counter
	outboxFailures  prometheus.Counter
	outboxLag       prometheus.Gauge
}

func NewAppMetrics(reg prometheus.Registerer) (*AppMetrics, error) {
	m := &AppMetrics{
		divergences: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wallet_reconciliation_divergence_total",
			Help: "Reconciliations whose stored balance differed from the ledger.",
		}),
		outboxPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_events_published_total",
			Help: "Outbox events sent to the broker and marked published.",
		}),
		outboxFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_publish_failures_total",
			Help: "Failed attempts to send an outbox event; the event is retried.",
		}),
		outboxLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_lag_seconds",
			Help: "Age of the oldest unpublished outbox event (0 when none).",
		}),
	}
	for _, c := range []prometheus.Collector{m.divergences, m.outboxPublished, m.outboxFailures, m.outboxLag} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *AppMetrics) ReconciliationDivergence() { m.divergences.Inc() }

func (m *AppMetrics) OutboxPublished()          { m.outboxPublished.Inc() }
func (m *AppMetrics) OutboxPublishFailed()      { m.outboxFailures.Inc() }
func (m *AppMetrics) OutboxLag(d time.Duration) { m.outboxLag.Set(d.Seconds()) }
