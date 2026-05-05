package registry

import (
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
}
