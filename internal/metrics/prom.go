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
)

var (
	Connections = prometheus.NewGauge(prometheus.GaugeOpts{Name: "wirefan_connections_total"})
	// Channels is defined for completeness but is NOT yet incremented in this
	// commit. Accurate channel counting requires a hook in the registry that
	// distinguishes create vs. lookup; that wiring lands with the
	// _wirefan-stats system channel (Task 22) or a registry-level callback.
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
