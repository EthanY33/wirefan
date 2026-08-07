package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// openRaw opens the database file directly, bypassing NewSQLite, so tests can
// fabricate pre-versioning states and inspect PRAGMA user_version.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	return db
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db := openRaw(t, path)
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

// makeV020Database builds a database in the exact shape wirefan v0.2.0 left
// behind: a bare keys table created with CREATE TABLE IF NOT EXISTS and
// user_version untouched at 0. It inserts one key and returns its id and
// secret hash.
func makeV020Database(t *testing.T, path string) (id, secretHash string) {
	t.Helper()
	db := openRaw(t, path)
	defer func() { _ = db.Close() }()
	// Verbatim v0.2.0 schema statement (internal/store/sqlite.go at v0.2.0).
	const v020Schema = `
CREATE TABLE IF NOT EXISTS keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    secret_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
);
`
	if _, err := db.Exec(v020Schema); err != nil {
		t.Fatalf("create v0.2.0 schema: %v", err)
	}
	id, secretHash = "01HV020LEGACYKEY0000000000", "deadbeefcafef00d"
	if _, err := db.Exec(`INSERT INTO keys(id,name,secret_hash,created_at) VALUES(?,?,?,?)`,
		id, "legacy", secretHash, int64(1714000000000)); err != nil {
		t.Fatalf("insert legacy key: %v", err)
	}
	return id, secretHash
}

func TestMigrateEmptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	k, err := s.CreateKey(context.Background(), "n", "hash")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	got, err := s.LookupKey(context.Background(), k.ID)
	if err != nil {
		t.Fatalf("LookupKey: %v", err)
	}
	if got.SecretHash != "hash" {
		t.Fatalf("SecretHash = %q, want %q", got.SecretHash, "hash")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if v := userVersion(t, path); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
}

func TestMigrateV020Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v020.db")
	id, secretHash := makeV020Database(t, path)

	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite on v0.2.0 database: %v", err)
	}
	defer func() { _ = s.Close() }()

	// The pre-existing key must survive migration with its secret hash
	// intact; this is what lets the key still authenticate after upgrade
	// (auth compares the stored hash against the presented secret).
	got, err := s.LookupKey(context.Background(), id)
	if err != nil {
		t.Fatalf("LookupKey after migration: %v", err)
	}
	if got.SecretHash != secretHash {
		t.Fatalf("SecretHash = %q, want %q", got.SecretHash, secretHash)
	}
	if got.RevokedAt != nil {
		t.Fatalf("RevokedAt = %v, want nil", got.RevokedAt)
	}

	// New writes must work alongside the adopted rows.
	if _, err := s.CreateKey(context.Background(), "post-upgrade", "h2"); err != nil {
		t.Fatalf("CreateKey after migration: %v", err)
	}
	keys, err := s.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("ListKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(keys))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if v := userVersion(t, path); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
}

func TestMigrateIdempotentReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
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

	for i := 0; i < 3; i++ {
		s, err = NewSQLite(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		if _, err := s.LookupKey(context.Background(), k.ID); err != nil {
			t.Fatalf("LookupKey after reopen %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if v := userVersion(t, path); v != 1 {
			t.Fatalf("user_version after reopen %d = %d, want 1", i, v)
		}
	}
}

func TestMigrateRefusesNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
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

	db := openRaw(t, path)
	if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	_, err = NewSQLite(path)
	if err == nil {
		t.Fatal("NewSQLite succeeded on a future-versioned database, want refusal")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Fatalf("error = %q, want mention of newer version", err)
	}

	// Refusal must not have modified the database.
	if v := userVersion(t, path); v != 999 {
		t.Fatalf("user_version after refusal = %d, want 999 untouched", v)
	}
	db = openRaw(t, path)
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM keys WHERE id=?`, k.ID).Scan(&n); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if n != 1 {
		t.Fatalf("key count after refusal = %d, want 1", n)
	}
}

func TestMigrateRefusesUnrecognizedKeysTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign.db")
	db := openRaw(t, path)
	// A version-0 database with a keys table that is NOT wirefan's: some
	// other application's file handed to --db-path by mistake.
	if _, err := db.Exec(`CREATE TABLE keys (kid INTEGER PRIMARY KEY, blob TEXT)`); err != nil {
		t.Fatalf("create foreign table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	_, err := NewSQLite(path)
	if err == nil {
		t.Fatal("NewSQLite succeeded on an unrecognized keys table, want refusal")
	}
	if !strings.Contains(err.Error(), "do not match the wirefan v0.2.0 schema") {
		t.Fatalf("error = %q, want schema-mismatch refusal", err)
	}
}

// TestMigrationRunnerMultiStep exercises the runner with a synthetic
// two-step list (the production list is a single baseline; see the comment
// on migrations). It proves ordered application, per-step version bumps,
// and that a second run is a no-op.
func TestMigrationRunnerMultiStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi.db")
	db := openRaw(t, path)
	defer func() { _ = db.Close() }()

	applied := []int{}
	ms := []migration{
		{version: 1, name: "baseline", apply: func(tx *sql.Tx) error {
			applied = append(applied, 1)
			_, err := tx.Exec(createKeysTableSQL)
			return err
		}},
		{version: 2, name: "add note column", apply: func(tx *sql.Tx) error {
			applied = append(applied, 2)
			_, err := tx.Exec(`ALTER TABLE keys ADD COLUMN note TEXT`)
			return err
		}},
	}

	if err := migrate(db, ms); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(applied) != 2 || applied[0] != 1 || applied[1] != 2 {
		t.Fatalf("applied = %v, want [1 2]", applied)
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	// The step-2 schema change must be visible.
	if _, err := db.Exec(`INSERT INTO keys(id,name,secret_hash,created_at,note) VALUES('a','b','c',0,'d')`); err != nil {
		t.Fatalf("insert using migrated column: %v", err)
	}

	// Second run: nothing to do, nothing re-applied.
	applied = applied[:0]
	if err := migrate(db, ms); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second run re-applied %v, want none", applied)
	}
}

// TestMigrationRunnerRollbackOnFailure proves a failing migration rolls back
// both its schema work and the version bump, leaving the database at the
// last fully applied version.
func TestMigrationRunnerRollbackOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	db := openRaw(t, path)
	defer func() { _ = db.Close() }()

	boom := errors.New("boom")
	ms := []migration{
		{version: 1, name: "baseline", apply: func(tx *sql.Tx) error {
			_, err := tx.Exec(createKeysTableSQL)
			return err
		}},
		{version: 2, name: "fails after writing", apply: func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO keys(id,name,secret_hash,created_at) VALUES('x','y','z',0)`); err != nil {
				return err
			}
			return boom
		}},
	}

	err := migrate(db, ms)
	if !errors.Is(err, boom) {
		t.Fatalf("migrate error = %v, want wrapped boom", err)
	}

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != 1 {
		t.Fatalf("user_version = %d, want 1 (failed step rolled back)", v)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM keys`).Scan(&n); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if n != 0 {
		t.Fatalf("keys rows = %d, want 0 (partial write rolled back)", n)
	}

	// Retrying with the failure fixed resumes from version 1.
	ms[1].apply = func(tx *sql.Tx) error {
		_, err := tx.Exec(`ALTER TABLE keys ADD COLUMN note TEXT`)
		return err
	}
	if err := migrate(db, ms); err != nil {
		t.Fatalf("retry migrate: %v", err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v != 2 {
		t.Fatalf("user_version after retry = %d, want 2", v)
	}
}
