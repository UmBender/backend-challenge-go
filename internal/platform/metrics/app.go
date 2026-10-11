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
	wagerResults    *prometheus.CounterVec
	wagerReplays    *prometheus.CounterVec
	wagerDuration   *prometheus.HistogramVec
	conflicts       prometheus.Counter
	pendingRetries  prometheus.Counter
	sqsMessages     *prometheus.CounterVec
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
	m.wagerResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wager_operations_total",
		Help: "Wager operations by origin (http, sqs, worker) and result (status or error class).",
	}, []string{"origin", "result"})
	m.wagerReplays = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "wager_duplicates_total",
		Help: "Repeated operations answered with the stored result (idempotent replays).",
	}, []string{"origin"})
	m.wagerDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "wager_processing_duration_seconds",
		Help:    "Time to process one wager operation, retries included.",
		Buckets: prometheus.DefBuckets,
	}, []string{"origin"})
	m.conflicts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wager_concurrency_conflicts_total",
		Help: "Transactions retried after a concurrency conflict detected by the database.",
	})
	m.pendingRetries = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "wager_pending_reference_retries_total",
		Help: "Lookups of a missing reference that rescheduled the operation.",
	})
	m.sqsMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sqs_messages_total",
		Help: "Input messages by outcome: processed, dlq (forwarded) or retry (made visible again).",
	}, []string{"outcome"})
	for _, c := range []prometheus.Collector{m.divergences, m.outboxPublished, m.outboxFailures, m.outboxLag,
		m.wagerResults, m.wagerReplays, m.wagerDuration, m.conflicts, m.pendingRetries, m.sqsMessages} {
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

func (m *AppMetrics) WagerResult(origin, result string, replay bool, took time.Duration) {
	m.wagerResults.WithLabelValues(origin, result).Inc()
	if replay {
		m.wagerReplays.WithLabelValues(origin).Inc()
	}
	m.wagerDuration.WithLabelValues(origin).Observe(took.Seconds())
}

func (m *AppMetrics) ConcurrencyConflict()      { m.conflicts.Inc() }
func (m *AppMetrics) PendingRetry()             { m.pendingRetries.Inc() }
func (m *AppMetrics) SQSMessage(outcome string) { m.sqsMessages.WithLabelValues(outcome).Inc() }
