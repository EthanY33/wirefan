package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Schema versioning uses SQLite's PRAGMA user_version.
//
// Version 0 is ambiguous by history: databases created by wirefan v0.2.0
// predate versioning entirely, so a user_version of 0 can mean either a
// genuinely empty database or a populated v0.2.0 database that already
// contains the keys table. The baseline migration is therefore written to
// adopt an existing v0.2.0 keys table in place (never drop or recreate it),
// and migrate refuses to proceed if a table named "keys" exists at version 0
// but does not match the v0.2.0 column set, because that is not a database
// wirefan created.
//
// Each migration runs inside a single transaction that also bumps
// user_version, so a crash mid-migration rolls back both the schema change
// and the version bump together; the database is always at a version whose
// migrations have fully applied.

// migration is one schema step. apply must be idempotent-safe only with
// respect to the state left by the previous version; the runner guarantees
// it executes at most once per database.
type migration struct {
	version int
	name    string
	apply   func(tx *sql.Tx) error
}

const createKeysTableSQL = `
CREATE TABLE IF NOT EXISTS keys (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    secret_hash TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
);
`

// migrations is the ordered, append-only list of schema steps. Versions must
// be contiguous starting at 1. The list currently holds only the baseline:
// no post-v0.2.0 schema change has been justified yet, and inventing one to
// exercise the runner would be worse than shipping the honest no-op. The
// runner itself is exercised against multi-step lists in migrate_test.go.
var migrations = []migration{
	{
		version: 1,
		name:    "baseline: keys table (v0.2.0 schema)",
		apply: func(tx *sql.Tx) error {
			// CREATE TABLE IF NOT EXISTS adopts a pre-versioning v0.2.0
			// database without touching its rows, and creates the table
			// fresh on an empty database. Both states converge to the
			// same version-1 schema.
			_, err := tx.Exec(createKeysTableSQL)
			return err
		},
	},
}

// v020KeysColumns is the exact column set of the keys table as shipped in
// v0.2.0, used to validate a pre-versioning database before adopting it.
var v020KeysColumns = []string{"created_at", "id", "name", "revoked_at", "secret_hash"}

// migrate brings db up to the newest version in ms, refusing to open
// databases from the future or version-0 databases whose keys table does not
// look like the v0.2.0 schema.
func migrate(db *sql.DB, ms []migration) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	latest := 0
	if len(ms) > 0 {
		latest = ms[len(ms)-1].version
	}
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (max %d); refusing to open; upgrade wirefan instead of downgrading the database", current, latest)
	}

	if current == 0 {
		// Pre-versioning state: either an empty database or one created by
		// v0.2.0. If a keys table already exists it must match the v0.2.0
		// shape exactly; anything else is not a wirefan key store and
		// running migrations over it could destroy foreign data.
		if err := checkPreVersioningKeysTable(db); err != nil {
			return err
		}
	}

	for _, m := range ms {
		if m.version <= current {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		current = m.version
	}
	return nil
}

// applyMigration runs one migration and the matching user_version bump in a
// single transaction so they commit or roll back together.
func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err := m.apply(tx); err != nil {
		return err
	}
	// PRAGMA does not support parameter binding; version is an int under our
	// control, never user input.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// checkPreVersioningKeysTable validates a version-0 database. An absent keys
// table means an empty database (fine). A present keys table must have
// exactly the v0.2.0 columns; otherwise refuse.
func checkPreVersioningKeysTable(db *sql.DB) error {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='keys'`).Scan(&n)
	if err != nil {
		return fmt.Errorf("inspect pre-versioning database: %w", err)
	}
	if n == 0 {
		return nil // genuinely empty database
	}

	rows, err := db.Query(`PRAGMA table_info(keys)`)
	if err != nil {
		return fmt.Errorf("inspect keys table: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cols []string
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notnull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("inspect keys table: %w", err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect keys table: %w", err)
	}
	sort.Strings(cols)

	if strings.Join(cols, ",") != strings.Join(v020KeysColumns, ",") {
		return fmt.Errorf("database has schema version 0 but its keys table columns (%s) do not match the wirefan v0.2.0 schema (%s); refusing to migrate an unrecognized database",
			strings.Join(cols, ", "), strings.Join(v020KeysColumns, ", "))
	}
	return nil
}
