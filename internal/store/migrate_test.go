package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileBytes snapshots the main database file so refusal tests can prove the
// refused file was left byte-for-byte unmodified.
func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

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

	before := fileBytes(t, path)
	_, err = NewSQLite(path)
	if err == nil {
		t.Fatal("NewSQLite succeeded on a future-versioned database, want refusal")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Fatalf("error = %q, want mention of newer version", err)
	}

	// Refusal must not have modified the database, byte for byte: the
	// preflight inspection runs over a read-only connection, never the WAL
	// handle that rewrites the journal-mode header.
	if after := fileBytes(t, path); !bytes.Equal(before, after) {
		t.Fatalf("refused open modified the database file (%d bytes before, %d after)", len(before), len(after))
	}
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
		{version: 1, name: "baseline", apply: func(tx execer) error {
			applied = append(applied, 1)
			_, err := tx.Exec(createKeysTableSQL)
			return err
		}},
		{version: 2, name: "add note column", apply: func(tx execer) error {
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
		{version: 1, name: "baseline", apply: func(tx execer) error {
			_, err := tx.Exec(createKeysTableSQL)
			return err
		}},
		{version: 2, name: "fails after writing", apply: func(tx execer) error {
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
	ms[1].apply = func(tx execer) error {
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

// TestMigrateConcurrentOpeners proves two processes racing to open the same
// file each apply a non-idempotent migration at most once. Without BEGIN
// IMMEDIATE and the in-transaction version re-read, every opener reads
// user_version 0 and every opener runs the ALTER TABLE; the losers fail
// with "duplicate column name: note".
func TestMigrateConcurrentOpeners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")
	ms := []migration{
		{version: 1, name: "baseline", apply: func(tx execer) error {
			_, err := tx.Exec(createKeysTableSQL)
			return err
		}},
		{version: 2, name: "add note", apply: func(tx execer) error {
			_, err := tx.Exec(`ALTER TABLE keys ADD COLUMN note TEXT`)
			return err
		}},
	}

	// Create the file (already in WAL mode) before the race: a restart
	// overlap or rolling deploy contends on an existing store, and racing
	// the initial WAL conversion of a brand-new file is a different,
	// test-only contention.
	setup := openRaw(t, path)
	if err := setup.Ping(); err != nil {
		t.Fatalf("create database file: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("close setup handle: %v", err)
	}

	const openers = 4
	start := make(chan struct{})
	errs := make(chan error, openers)
	for i := 0; i < openers; i++ {
		go func() {
			db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = db.Close() }()
			<-start
			errs <- migrate(db, ms)
		}()
	}
	close(start)
	for i := 0; i < openers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent opener returned error: %v", err)
		}
	}

	if v := userVersion(t, path); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	// The note column must exist exactly once.
	db := openRaw(t, path)
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO keys(id,name,secret_hash,created_at,note) VALUES('a','b','c',0,'d')`); err != nil {
		t.Fatalf("insert using migrated column: %v", err)
	}
}

// TestMigrateRefusesForeignDatabase proves that a version-0 SQLite file with
// no keys table at all (some other application's database handed to
// --db-path by mistake) is refused rather than adopted, and that the refusal
// leaves the file byte-for-byte unmodified.
func TestMigrateRefusesForeignDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreignapp.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE invoices (id INTEGER PRIMARY KEY, amount REAL)`); err != nil {
		t.Fatalf("create foreign table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO invoices(amount) VALUES (12.5)`); err != nil {
		t.Fatalf("insert foreign row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	before := fileBytes(t, path)
	_, err = NewSQLite(path)
	if err == nil {
		t.Fatal("NewSQLite succeeded on a foreign database, want refusal")
	}
	if !strings.Contains(err.Error(), "tables wirefan did not create") {
		t.Fatalf("error = %q, want foreign-table refusal", err)
	}
	if after := fileBytes(t, path); !bytes.Equal(before, after) {
		t.Fatalf("refused open modified the foreign database file (%d bytes before, %d after)", len(before), len(after))
	}
}

// TestMigrateRefusesConstraintlessKeysTable proves the v0.2.0 shape check
// covers constraints, not just column names: a keys table without the
// PRIMARY KEY on id would accept duplicate ids and make which row
// authenticates unspecified, so it must be refused.
func TestMigrateRefusesConstraintlessKeysTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noconstraints.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE keys (id TEXT, name TEXT, secret_hash TEXT, created_at INTEGER, revoked_at INTEGER)`); err != nil {
		t.Fatalf("create constraint-free table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = NewSQLite(path)
	if err == nil {
		t.Fatal("NewSQLite succeeded on a constraint-free keys table, want refusal")
	}
	if !strings.Contains(err.Error(), "do not match the wirefan v0.2.0 schema") {
		t.Fatalf("error = %q, want schema-mismatch refusal", err)
	}
}

// TestMigrateRejectsNonContiguousVersions proves the runner refuses a
// migration list whose versions are not contiguous from 1, so a copied
// step with an unbumped or skipped version fails loudly instead of
// silently never running.
func TestMigrateRejectsNonContiguousVersions(t *testing.T) {
	noop := func(tx execer) error { return nil }
	cases := map[string][]migration{
		"gap":            {{version: 1, name: "a", apply: noop}, {version: 3, name: "b", apply: noop}},
		"duplicate":      {{version: 1, name: "a", apply: noop}, {version: 2, name: "b", apply: noop}, {version: 2, name: "c", apply: noop}},
		"starts at zero": {{version: 0, name: "a", apply: noop}},
	}
	for name, ms := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "contig.db")
			db := openRaw(t, path)
			defer func() { _ = db.Close() }()
			err := migrate(db, ms)
			if err == nil {
				t.Fatal("migrate accepted a non-contiguous migration list")
			}
			if !strings.Contains(err.Error(), "contiguous from 1") {
				t.Fatalf("error = %q, want contiguity refusal", err)
			}
		})
	}
}
