package store

import (
	"path/filepath"
	"testing"
)

func TestSQLiteStore(t *testing.T) {
	runStoreTests(t, func(t *testing.T) Store {
		path := filepath.Join(t.TempDir(), "test.db")
		s, err := NewSQLite(path)
		if err != nil {
			t.Fatalf("NewSQLite: %v", err)
		}
		return s
	})
}
