package store

import (
	"context"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

type memoryStore struct {
	mu   sync.RWMutex
	keys map[string]Key
}

func NewMemory() Store { return &memoryStore{keys: map[string]Key{}} }

func (m *memoryStore) CreateKey(_ context.Context, name, secretHash string) (Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := Key{ID: ulid.Make().String(), Name: name, SecretHash: secretHash, CreatedAt: time.Now().UTC()}
	m.keys[k.ID] = k
	return k, nil
}

func (m *memoryStore) LookupKey(_ context.Context, id string) (Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok {
		return Key{}, ErrKeyNotFound
	}
	return k, nil
}

func (m *memoryStore) ListKeys(_ context.Context) ([]Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Key, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, k)
	}
	return out, nil
}

func (m *memoryStore) RevokeKey(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return ErrKeyNotFound
	}
	now := time.Now().UTC()
	k.RevokedAt = &now
	m.keys[id] = k
	return nil
}

func (m *memoryStore) Close() error { return nil }
