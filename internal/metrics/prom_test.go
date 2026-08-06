package metrics

import "testing"

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
