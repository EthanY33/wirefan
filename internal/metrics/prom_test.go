package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestSnapshotBasicTracksCollectors mutates the live collectors and checks
// the snapshot reflects them. Deltas (not absolutes) are asserted because
// the collectors are package-level singletons shared with other tests.
func TestSnapshotBasicTracksCollectors(t *testing.T) {
	before := SnapshotBasic()

	Connections.Inc()
	Connections.Inc()
	Published.Inc()
	Dropped.WithLabelValues("slow_consumer").Inc()
	Dropped.WithLabelValues("closed").Inc()

	after := SnapshotBasic()
	if got := after["connections"] - before["connections"]; got != 2 {
		t.Errorf("connections delta = %d, want 2", got)
	}
	if got := after["messages_published_total"] - before["messages_published_total"]; got != 1 {
		t.Errorf("published delta = %d, want 1", got)
	}
	// "published" is the demo-tile key; it must always mirror the
	// Prometheus-shaped alias.
	if after["published"] != after["messages_published_total"] {
		t.Errorf("published = %d, want alias of messages_published_total = %d",
			after["published"], after["messages_published_total"])
	}
	// "dropped" sums every reason label of the CounterVec.
	if got := after["dropped"] - before["dropped"]; got != 2 {
		t.Errorf("dropped delta = %d, want 2", got)
	}

	Connections.Dec()
	Connections.Dec()

	final := SnapshotBasic()
	if got := final["connections"]; got != before["connections"] {
		t.Errorf("connections = %d after restore, want %d", got, before["connections"])
	}
}

// TestChannelSourceBacksGaugeAndSnapshot proves wirefan_channels is
// wired to a live source rather than reporting a constant 0, which is what
// it did before the source callback existed.
func TestChannelSourceBacksGaugeAndSnapshot(t *testing.T) {
	t.Cleanup(func() { SetChannelSource(nil) })

	SetChannelSource(nil)
	if got := channelCount(); got != 0 {
		t.Errorf("channelCount with no source = %v, want 0", got)
	}
	if got := SnapshotBasic()["channels"]; got != 0 {
		t.Errorf("snapshot channels with no source = %d, want 0", got)
	}

	n := 3
	SetChannelSource(func() int { return n })
	if got := channelCount(); got != 3 {
		t.Errorf("channelCount = %v, want 3", got)
	}
	if got := SnapshotBasic()["channels"]; got != 3 {
		t.Errorf("snapshot channels = %d, want 3", got)
	}

	// Read at call time, not captured at install time: the whole point of a
	// callback is that channel churn is reflected without re-registration.
	n = 7
	if got := channelCount(); got != 7 {
		t.Errorf("channelCount after change = %v, want 7", got)
	}
	if got := SnapshotBasic()["channels"]; got != 7 {
		t.Errorf("snapshot channels after change = %d, want 7", got)
	}
}

// TestExpositionNamesAndHelp pins the /metrics surface that 1.0 freezes:
// the exact family names and types, a non-empty Help on every family, and
// the Prometheus naming rule that only counters carry the _total suffix. A
// gauge named *_total reads as a counter to PromQL users and tooling (rate()
// over it is meaningless), which is what wirefan_connections_total and
// wirefan_channels_total did before the rename.
func TestExpositionNamesAndHelp(t *testing.T) {
	Register()
	// A Vec exposes no family until at least one label combination exists.
	Dropped.WithLabelValues("slow_consumer")
	UpgradeRej.WithLabelValues("bad_key")

	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]*dto.MetricFamily{}
	for _, f := range fams {
		if strings.HasPrefix(f.GetName(), "wirefan_") {
			got[f.GetName()] = f
		}
	}

	want := map[string]dto.MetricType{
		"wirefan_connections":               dto.MetricType_GAUGE,
		"wirefan_channels":                  dto.MetricType_GAUGE,
		"wirefan_messages_published_total":  dto.MetricType_COUNTER,
		"wirefan_messages_dropped_total":    dto.MetricType_COUNTER,
		"wirefan_broadcast_latency_seconds": dto.MetricType_HISTOGRAM,
		"wirefan_upgrade_rejected_total":    dto.MetricType_COUNTER,
		"wirefan_auth_failures_total":       dto.MetricType_COUNTER,
	}
	for name, typ := range want {
		f, ok := got[name]
		if !ok {
			t.Errorf("family %s not exposed", name)
			continue
		}
		if f.GetType() != typ {
			t.Errorf("family %s type = %v, want %v", name, f.GetType(), typ)
		}
		if strings.TrimSpace(f.GetHelp()) == "" {
			t.Errorf("family %s has no Help text", name)
		}
	}
	for name, f := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected family %s exposed", name)
		}
		if strings.HasSuffix(name, "_total") && f.GetType() != dto.MetricType_COUNTER {
			t.Errorf("family %s is a %v but carries the counter-only _total suffix", name, f.GetType())
		}
	}
}
