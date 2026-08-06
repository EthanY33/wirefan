// Package metrics defines wirefan's Prometheus collectors and the OTel hook.
//
// All collectors are package-level singletons so call sites can emit values
// without plumbing a registry through every constructor. Register() must be
// called once during server boot to register them with the default registry;
// it is wrapped in sync.Once so test suites that boot multiple Server
// instances in-process don't trip prometheus.MustRegister's panic-on-dup.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

var (
	Connections = prometheus.NewGauge(prometheus.GaugeOpts{Name: "wirefan_connections_total"})
	// Channels is defined for completeness but is NOT incremented anywhere.
	// Accurate channel counting requires a hook in the registry that
	// distinguishes create vs. lookup; the _wirefan-stats publisher instead
	// reads the registry's Len() directly (see cmd/wirefan), which is the
	// source of truth. Wiring this gauge would need a registry-level callback.
	Channels  = prometheus.NewGauge(prometheus.GaugeOpts{Name: "wirefan_channels_total"})
	Published = prometheus.NewCounter(prometheus.CounterOpts{Name: "wirefan_messages_published_total"})
	Dropped   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wirefan_messages_dropped_total"}, []string{"reason"})
	Latency   = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "wirefan_broadcast_latency_seconds",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 16),
	})
	UpgradeRej = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "wirefan_upgrade_rejected_total"}, []string{"reason"})
	AuthFails  = prometheus.NewCounter(prometheus.CounterOpts{Name: "wirefan_auth_failures_total"})
)

// readValue extracts the current value from a gauge or counter collector by
// serializing it into the wire DTO. This is the same data /metrics exposes,
// read in-process, so _wirefan-stats snapshots always agree with Prometheus.
func readValue(m prometheus.Metric) float64 {
	var d dto.Metric
	if err := m.Write(&d); err != nil {
		return 0
	}
	if d.Gauge != nil {
		return d.Gauge.GetValue()
	}
	if d.Counter != nil {
		return d.Counter.GetValue()
	}
	return 0
}

// readVecTotal sums every child of a CounterVec (all label combinations).
// Collect is used because a Vec has no single value to Write; each child is
// still read from the same collectors /metrics exposes.
func readVecTotal(vec *prometheus.CounterVec) float64 {
	ch := make(chan prometheus.Metric)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	var total float64
	for m := range ch {
		total += readValue(m)
	}
	return total
}

// SnapshotBasic returns the counters the _wirefan-stats system channel
// publishes. The "connections", "published", and "dropped" keys are a
// contract with the demo client's stat tiles (web/index.html data-stat
// attributes); "messages_published_total" is kept as a Prometheus-shaped
// alias of "published". Channel count comes from the registry (see
// cmd/wirefan) because the Channels gauge is not wired to registry
// create/delete events.
func SnapshotBasic() map[string]int64 {
	published := int64(readValue(Published))
	return map[string]int64{
		"connections":              int64(readValue(Connections)),
		"published":                published,
		"messages_published_total": published,
		"dropped":                  int64(readVecTotal(Dropped)),
	}
}

var registerOnce sync.Once

// Register registers all wirefan metrics with the default Prometheus registry.
// Idempotent: subsequent calls are no-ops thanks to sync.Once.
func Register() {
	registerOnce.Do(realRegister)
}

func realRegister() {
	prometheus.MustRegister(
		Connections,
		Channels,
		Published,
		Dropped,
		Latency,
		UpgradeRej,
		AuthFails,
	)
}
