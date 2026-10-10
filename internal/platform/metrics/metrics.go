// Package metrics owns the Prometheus registry. A dedicated registry (instead
// of the global default) keeps tests isolated and makes every exported metric
// an explicit registration.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.uber.org/fx"
)

var Module = fx.Module("metrics",
	fx.Provide(
		NewRegistry,
		func(r *prometheus.Registry) prometheus.Registerer { return r },
		func(r *prometheus.Registry) prometheus.Gatherer { return r },
	),
)

func NewRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return r
}
