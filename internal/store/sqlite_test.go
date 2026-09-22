package store

import (
	"context"
	"os"
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

// TestSQLiteReopenEscapesPreflightPath reopens a store whose path holds
// characters with URI meaning. The preflight travels as a file: URI, so an
// unescaped "%41" would be decoded to "A" and the read-only open would miss
// the real file (mode=ro cannot create one) and fail the reopen.
func TestSQLiteReopenEscapesPreflightPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "odd dir %41", "key store %2F.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	k, err := s.CreateKey(context.Background(), "n", "hash")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = NewSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.LookupKey(context.Background(), k.ID); err != nil {
		t.Fatalf("LookupKey after reopen: %v", err)
	}
}
