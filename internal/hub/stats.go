package hub

import (
	"context"
	"encoding/json"
	"time"

	"github.com/EthanY33/wirefan/internal/registry"
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
				"id":      time.Now().Format(time.RFC3339Nano),
			})
			Broadcast(ch, payload)
		}
	}
}
