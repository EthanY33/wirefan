package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
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
// and a version-0 database is refused unless it is either empty or contains
// exactly one table, keys, matching the v0.2.0 shape (constraints included),
// because anything else is not a database wirefan created.
//
// Each migration runs inside a single transaction that also bumps
// user_version. The transaction is opened with BEGIN IMMEDIATE and re-reads
// user_version after taking the write lock, so two processes racing to open
// the same file apply each migration at most once, and a crash mid-migration
// rolls back the schema change and the version bump together.
//
// Refusals are read-only: NewSQLite inspects an existing file over a
// query-only preflight connection before the writable WAL handle is opened,
// so a refused file is left byte-for-byte unmodified.

// execer is the statement surface a migration step gets: the
// transaction-scoped connection that applyMigration drives with
// BEGIN IMMEDIATE.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// migration is one schema step. apply must be idempotent-safe only with
// respect to the state left by the previous version; the runner guarantees
// it executes at most once per database.
type migration struct {
	version int
	name    string
	apply   func(tx execer) error
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
// be contiguous starting at 1; validateMigrations enforces this on every
// open, so a copy-pasted step with an unbumped version fails loudly instead
// of silently never running. The list currently holds only the baseline:
// no post-v0.2.0 schema change has been justified yet, and inventing one to
// exercise the runner would be worse than shipping the honest no-op. The
// runner itself is exercised against multi-step lists in migrate_test.go.
var migrations = []migration{
	{
		version: 1,
		name:    "baseline: keys table (v0.2.0 schema)",
		apply: func(tx execer) error {
			// CREATE TABLE IF NOT EXISTS adopts a pre-versioning v0.2.0
			// database without touching its rows, and creates the table
			// fresh on an empty database. Both states converge to the
			// same version-1 schema.
			_, err := tx.Exec(createKeysTableSQL)
			return err
		},
	},
}

// keyColumn is one column's shape as reported by PRAGMA table_info: name
// plus the NOT NULL and primary-key flags.
type keyColumn struct {
	name    string
	notNull int
	pk      int
}

// v020KeysColumns is the exact column shape of the keys table as shipped in
// v0.2.0, used to validate a pre-versioning database before adopting it.
// Names alone are not enough: a keys table missing the PRIMARY KEY on id
// would accept duplicate ids, making which row authenticates unspecified.
var v020KeysColumns = []keyColumn{
	{"created_at", 1, 0},
	{"id", 0, 1},
	{"name", 1, 0},
	{"revoked_at", 0, 0},
	{"secret_hash", 1, 0},
}

// validateMigrations enforces the append-only contract structurally:
// versions must be contiguous starting at 1.
func validateMigrations(ms []migration) error {
	for i, m := range ms {
		if m.version != i+1 {
			return fmt.Errorf("migration list invalid at index %d: version %d, want %d (versions must be contiguous from 1)", i, m.version, i+1)
		}
	}
	return nil
}

// preflight inspects an existing database file over a query-only connection
// before the writable handle is opened. The writable DSN converts the file
// to WAL journaling as a side effect of merely opening it, which would
// rewrite header bytes even when migrate then refuses; the refusal paths
// must leave a file wirefan does not own untouched.
func preflight(path string, ms []migration) error {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // fresh database; nothing to inspect
	}
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return nil // zero-byte file; SQLite treats it as an empty database
	}
	db, err := sql.Open("sqlite3", path+"?_query_only=true&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	_, err = checkDatabase(db, ms)
	return err
}

// migrate brings db up to the newest version in ms, refusing databases from
// the future and version-0 databases wirefan did not create.
func migrate(db *sql.DB, ms []migration) error {
	if err := validateMigrations(ms); err != nil {
		return err
	}
	current, err := checkDatabase(db, ms)
	if err != nil {
		return err
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

// checkDatabase reads the schema version and refuses databases wirefan
// cannot own: future versions, and version-0 files that are neither empty
// nor a v0.2.0 key store. It only reads, so preflight can run it over a
// query-only connection.
func checkDatabase(db *sql.DB, ms []migration) (int, error) {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	latest := 0
	if len(ms) > 0 {
		latest = ms[len(ms)-1].version
	}
	if current > latest {
		return 0, fmt.Errorf("database schema version %d is newer than this binary supports (max %d); refusing to open; upgrade wirefan instead of downgrading the database", current, latest)
	}
	if current == 0 {
		if err := checkPreVersioningDatabase(db); err != nil {
			return 0, err
		}
	}
	return current, nil
}

// applyMigration runs one migration and the matching user_version bump in a
// single transaction so they commit or roll back together. The transaction
// is opened with BEGIN IMMEDIATE so the write lock is held before the
// version is re-read: two processes racing to open the same file serialize
// here, and the loser sees the winner's version bump and skips.
// database/sql's Begin cannot be used because the driver issues BEGIN
// DEFERRED, which takes no lock until the first write.
func applyMigration(db *sql.DB, m migration) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		}
	}()

	// Re-read the version now that the write lock is held: a concurrent
	// opener may have applied this migration between the unlocked read in
	// checkDatabase and this transaction.
	var current int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return err
	}
	if current >= m.version {
		return nil // already applied by a concurrent opener
	}

	if err := m.apply(connTx{ctx: ctx, conn: conn}); err != nil {
		return err
	}
	// PRAGMA does not support parameter binding; version is an int under our
	// control, never user input.
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// connTx adapts the dedicated migration connection to the execer surface
// migration steps use, scoping every statement to the explicit transaction
// applyMigration drives on that connection.
type connTx struct {
	ctx  context.Context
	conn *sql.Conn
}

func (c connTx) Exec(query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(c.ctx, query, args...)
}

// checkPreVersioningDatabase validates a version-0 database. No tables at
// all means a genuinely empty database (fine). Exactly one table named
// "keys" is validated against the v0.2.0 shape. Any other table means the
// file belongs to some other application: running migrations over it would
// write wirefan's table into a foreign database and clobber its
// user_version, so refuse.
func checkPreVersioningDatabase(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("inspect pre-versioning database: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("inspect pre-versioning database: %w", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect pre-versioning database: %w", err)
	}

	switch {
	case len(tables) == 0:
		return nil // genuinely empty database
	case len(tables) == 1 && tables[0] == "keys":
		return checkV020KeysTable(db)
	default:
		return fmt.Errorf("database has schema version 0 but contains tables wirefan did not create (%s); refusing to migrate a database that belongs to another application", strings.Join(tables, ", "))
	}
}

// checkV020KeysTable compares the keys table's columns, NOT NULL flags, and
// primary key against the exact v0.2.0 shape; anything else is refused.
func checkV020KeysTable(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(keys)`)
	if err != nil {
		return fmt.Errorf("inspect keys table: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cols []keyColumn
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
		cols = append(cols, keyColumn{name: name, notNull: notnull, pk: pk})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect keys table: %w", err)
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })

	if formatColumns(cols) != formatColumns(v020KeysColumns) {
		return fmt.Errorf("database has schema version 0 but its keys table columns (%s) do not match the wirefan v0.2.0 schema (%s); refusing to migrate an unrecognized database",
			formatColumns(cols), formatColumns(v020KeysColumns))
	}
	return nil
}

// formatColumns renders a column shape for comparison and error messages,
// e.g. "created_at notnull, id pk, name notnull, ...".
func formatColumns(cols []keyColumn) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		s := c.name
		if c.pk != 0 {
			s += " pk"
		}
		if c.notNull != 0 {
			s += " notnull"
		}
		parts[i] = s
	}
	return strings.Join(parts, ", ")
}
