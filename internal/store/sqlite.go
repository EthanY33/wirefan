package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/oklog/ulid/v2"
)

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    secret_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
);
`

type sqliteStore struct{ db *sql.DB }

// validateDBPath rejects values that would let a misconfigured operator
// smuggle additional DSN parameters or pick a different DB. The --db-path
// flag flows operator config straight into here, so the guard catches
// operator mistakes before they become attack surface.
func validateDBPath(path string) error {
	if path == "" {
		return errors.New("db path is empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("db path must be absolute (got %q)", path)
	}
	if strings.ContainsAny(path, "?#\x00\n\r") {
		return fmt.Errorf("db path contains forbidden character (got %q)", path)
	}
	return nil
}

func NewSQLite(path string) (Store, error) {
	if err := validateDBPath(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) CreateKey(ctx context.Context, name, secretHash string) (Key, error) {
	k := Key{ID: ulid.Make().String(), Name: name, SecretHash: secretHash, CreatedAt: time.Now().UTC()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO keys(id,name,secret_hash,created_at) VALUES(?,?,?,?)`,
		k.ID, k.Name, k.SecretHash, k.CreatedAt.UnixMilli())
	return k, err
}

func (s *sqliteStore) LookupKey(ctx context.Context, id string) (Key, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,secret_hash,created_at,revoked_at FROM keys WHERE id=?`, id)
	var k Key
	var createdMs int64
	var revokedMs sql.NullInt64
	err := row.Scan(&k.ID, &k.Name, &k.SecretHash, &createdMs, &revokedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrKeyNotFound
	}
	if err != nil {
		return Key{}, err
	}
	k.CreatedAt = time.UnixMilli(createdMs).UTC()
	if revokedMs.Valid {
		t := time.UnixMilli(revokedMs.Int64).UTC()
		k.RevokedAt = &t
	}
	return k, nil
}

func (s *sqliteStore) ListKeys(ctx context.Context) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,secret_hash,created_at,revoked_at FROM keys`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Key
	for rows.Next() {
		var k Key
		var createdMs int64
		var revokedMs sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &k.SecretHash, &createdMs, &revokedMs); err != nil {
			return nil, err
		}
		k.CreatedAt = time.UnixMilli(createdMs).UTC()
		if revokedMs.Valid {
			t := time.UnixMilli(revokedMs.Int64).UTC()
			k.RevokedAt = &t
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *sqliteStore) RevokeKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE keys SET revoked_at=? WHERE id=?`, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }
