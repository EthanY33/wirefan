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

	after := SnapshotBasic()
	if got := after["connections"] - before["connections"]; got != 2 {
		t.Errorf("connections delta = %d, want 2", got)
	}
	if got := after["messages_published_total"] - before["messages_published_total"]; got != 1 {
		t.Errorf("published delta = %d, want 1", got)
	}

	Connections.Dec()
	Connections.Dec()

	final := SnapshotBasic()
	if got := final["connections"]; got != before["connections"] {
		t.Errorf("connections = %d after restore, want %d", got, before["connections"])
	}
}
