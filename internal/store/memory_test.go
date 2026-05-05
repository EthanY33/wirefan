package store

import "testing"

func TestMemoryStore(t *testing.T) {
	runStoreTests(t, func(*testing.T) Store { return NewMemory() })
}
