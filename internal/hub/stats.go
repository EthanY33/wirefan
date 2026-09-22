package hub

import (
	"context"
	"encoding/json"
	"time"

	"github.com/EthanY33/wirefan/internal/registry"
	"github.com/oklog/ulid/v2"
)

// PublishStatsLoop periodically publishes a snapshot to the reserved
// "_wirefan-stats" channel. snap() should return the live metric values.
// Returns when ctx is canceled.
func PublishStatsLoop(ctx context.Context, r registry.Registry, interval time.Duration, snap func() map[string]int64) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ch := r.GetOrCreate("_wirefan-stats")
			payload, _ := json.Marshal(map[string]any{
				"type":    "event",
				"channel": "_wirefan-stats",
				"data":    snap(),
				// A ULID like every other event id, so clients can
				// treat ids uniformly (and still sort them by time).
				"id": ulid.Make().String(),
			})
			Broadcast(ch, payload)
		}
	}
}
