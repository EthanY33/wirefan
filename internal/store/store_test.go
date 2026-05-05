package store

import (
	"context"
	"errors"
	"testing"
)

func runStoreTests(t *testing.T, factory func(t *testing.T) Store) {
	t.Run("CreateAndLookup", func(t *testing.T) {
		s := factory(t)
		defer s.Close()
		ctx := context.Background()
		k, err := s.CreateKey(ctx, "test", "hash1")
		if err != nil {
			t.Fatalf("CreateKey: %v", err)
		}
		got, err := s.LookupKey(ctx, k.ID)
		if err != nil {
			t.Fatalf("LookupKey: %v", err)
		}
		if got.Name != "test" || got.SecretHash != "hash1" {
			t.Errorf("got %+v", got)
		}
	})
	t.Run("LookupMissing", func(t *testing.T) {
		s := factory(t)
		defer s.Close()
		_, err := s.LookupKey(context.Background(), "nope")
		if !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("want ErrKeyNotFound, got %v", err)
		}
	})
	t.Run("Revoke", func(t *testing.T) {
		s := factory(t)
		defer s.Close()
		ctx := context.Background()
		k, _ := s.CreateKey(ctx, "x", "h")
		if err := s.RevokeKey(ctx, k.ID); err != nil {
			t.Fatal(err)
		}
		got, err := s.LookupKey(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RevokedAt == nil {
			t.Fatal("expected RevokedAt set")
		}
	})
	t.Run("List", func(t *testing.T) {
		s := factory(t)
		defer s.Close()
		ctx := context.Background()
		s.CreateKey(ctx, "a", "h1")
		s.CreateKey(ctx, "b", "h2")
		got, err := s.ListKeys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("want 2 keys, got %d", len(got))
		}
	})
}
