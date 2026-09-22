package registry

import (
	"strconv"
	"sync"
	"testing"
)

func runRegistryTests(t *testing.T, factory func() Registry) {
	t.Run("GetOrCreate", func(t *testing.T) {
		r := factory()
		c1 := r.GetOrCreate("a")
		c2 := r.GetOrCreate("a")
		if c1 != c2 {
			t.Fatal("GetOrCreate must return same instance")
		}
		if r.Len() != 1 {
			t.Fatalf("Len=%d", r.Len())
		}
	})
	t.Run("LookupMissing", func(t *testing.T) {
		r := factory()
		if _, ok := r.Lookup("nope"); ok {
			t.Fatal("expected not ok")
		}
	})
	t.Run("Delete", func(t *testing.T) {
		r := factory()
		r.GetOrCreate("a")
		r.Delete("a")
		if _, ok := r.Lookup("a"); ok {
			t.Fatal("expected gone")
		}
	})
	t.Run("ConcurrentGetOrCreate", func(t *testing.T) {
		r := factory()
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.GetOrCreate("shared")
			}()
		}
		wg.Wait()
		if r.Len() != 1 {
			t.Fatalf("expected 1, got %d", r.Len())
		}
	})
	t.Run("SweepNeverHandsOutDeleted", func(t *testing.T) {
		// A subscriber racing the sweep calls GetOrCreate and then checks
		// Deleted under SubsMu, retrying a bounded number of times. If a
		// channel Sweep has marked Deleted is still in the registry, every
		// retry gets the same dead channel back and the subscribe fails.
		// The probe runs right after Sweep releases each channel's SubsMu,
		// which is exactly when such a subscriber can get in.
		r := factory()
		for _, name := range []string{"a", "b", "c", "d"} {
			r.GetOrCreate(name)
		}
		swept := 0
		Sweep(sweepProbe{Registry: r, after: func(c *Channel) {
			if !c.Deleted.Load() {
				return
			}
			swept++
			if got, ok := r.Lookup(c.Name); ok && got == c {
				t.Errorf("Lookup(%q) returned the channel Sweep just marked Deleted", c.Name)
			}
			if got := r.GetOrCreate(c.Name); got == c || got.Deleted.Load() {
				t.Errorf("GetOrCreate(%q) handed out a Deleted channel mid-sweep", c.Name)
			}
		}})
		if swept == 0 {
			t.Fatal("probe saw no swept channel")
		}
	})
	t.Run("CompareAndDelete", func(t *testing.T) {
		r := factory()
		c := r.GetOrCreate("a")
		if r.CompareAndDelete("a", newChannel("a")) {
			t.Fatal("removed an entry that maps to a different channel")
		}
		if got, ok := r.Lookup("a"); !ok || got != c {
			t.Fatal("a mismatched CompareAndDelete disturbed the live entry")
		}
		if !r.CompareAndDelete("a", c) {
			t.Fatal("did not remove the matching entry")
		}
		if _, ok := r.Lookup("a"); ok {
			t.Fatal("entry still present after CompareAndDelete")
		}
		if r.CompareAndDelete("a", c) {
			t.Fatal("reported removing an absent entry")
		}
	})
	t.Run("SubscribeRacingSweep", func(t *testing.T) {
		// Subscribers follow hub.Subscribe's protocol (GetOrCreate, then
		// check Deleted under SubsMu and retry) while Sweep runs in a
		// tight loop. Run under -race this guards the SubsMu -> registry
		// lock order Sweep now uses; the end state proves no successful
		// subscriber was stranded on a channel the registry dropped.
		r := factory()
		stop := make(chan struct{})
		sweeperDone := make(chan struct{})
		go func() {
			defer close(sweeperDone)
			for {
				select {
				case <-stop:
					return
				default:
					Sweep(r)
				}
			}
		}()

		const workers, perWorker = 8, 200
		type joined struct {
			sub stubSub
			ch  *Channel
		}
		results := make([][]joined, workers)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					s := stubSub{id: w*perWorker + i}
					name := "ch" + strconv.Itoa(i%4)
					// The cap only bounds a pathological livelock: a tight
					// sweeper can reap a fresh empty channel before the
					// subscriber locks it, which the minute-scale
					// production sweep cannot do in practice.
					for try := 0; ; try++ {
						if try == 1000 {
							t.Errorf("subscribe to %s never landed on a live channel", name)
							return
						}
						c := r.GetOrCreate(name)
						c.SubsMu.Lock()
						ok := !c.Deleted.Load()
						if ok {
							c.Subscribers[s] = struct{}{}
						}
						c.SubsMu.Unlock()
						if ok {
							results[w] = append(results[w], joined{s, c})
							break
						}
					}
				}
			}()
		}
		wg.Wait()
		close(stop)
		<-sweeperDone

		for _, rs := range results {
			for _, j := range rs {
				if j.ch.Deleted.Load() {
					t.Fatalf("subscriber %d sits on a channel marked Deleted", j.sub.id)
				}
				if got, ok := r.Lookup(j.ch.Name); !ok || got != j.ch {
					t.Fatalf("subscriber %d sits on a channel the registry no longer maps %q to", j.sub.id, j.ch.Name)
				}
			}
		}
	})
}

// stubSub is a Subscriber with an identity, so distinct values are distinct
// Subscribers map keys.
type stubSub struct{ id int }

func (stubSub) Send([]byte) error { return nil }
func (stubSub) Close()            {}

// sweepProbe wraps a Registry so a test can observe the registry between
// Sweep's per-channel steps: after runs once Sweep's callback for a channel
// has returned (and so released that channel's SubsMu), before Range moves
// on to the next channel.
type sweepProbe struct {
	Registry
	after func(*Channel)
}

func (p sweepProbe) Range(fn func(*Channel) bool) {
	p.Registry.Range(func(c *Channel) bool {
		more := fn(c)
		p.after(c)
		return more
	})
}
