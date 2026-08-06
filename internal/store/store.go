package store

import (
	"context"
	"errors"
	"time"
)

var ErrKeyNotFound = errors.New("key not found")

type Key struct {
	ID         string
	Name       string
	SecretHash string
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

type Store interface {
	CreateKey(ctx context.Context, name, secretHash string) (Key, error)
	LookupKey(ctx context.Context, id string) (Key, error)
	ListKeys(ctx context.Context) ([]Key, error)
	RevokeKey(ctx context.Context, id string) error
	Close() error
}
