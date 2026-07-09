// Package store owns the single embedded SQLite database (ADR-0007).
//
// Driver choice: modernc.org/sqlite — a pure-Go (CGO-free) SQLite. This keeps
// `go build` and the Docker image simple (no C toolchain, trivial cross-compile
// to arm64 per ADR-0006/0011) while still supporting WAL mode and FTS5, both of
// which this slice requires. A CGO driver (mattn/go-sqlite3) would be marginally
// faster but reintroduces a C toolchain into every build and cross-compile,
// which works against the low-friction Docker-first posture.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DB wraps the *sql.DB handle for the single embedded database.
type DB struct {
	*sql.DB
}

// Open opens (creating if absent) the SQLite database at path with WAL mode
// enabled and reasonable pragmas for a single-process server. The returned DB
// has not yet had migrations applied; call Migrate for that.
func Open(path string) (*DB, error) {
	// modernc.org/sqlite accepts PRAGMAs as connection query params via the
	// _pragma key, applied to every pooled connection. WAL + foreign keys +
	// a busy timeout cover the single-writer concurrency model of ADR-0007.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: opening sqlite at %q: %w", path, err)
	}

	// SQLite is single-writer; serialize writers but keep the handle usable.
	// A single open connection avoids "database is locked" surprises under WAL
	// for this write volume and keeps the in-memory test DB (one connection)
	// from being dropped between statements.
	sqldb.SetMaxOpenConns(1)

	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("store: pinging sqlite at %q: %w", path, err)
	}

	db := &DB{DB: sqldb}
	if err := db.verifyCapabilities(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return db, nil
}

// verifyCapabilities confirms WAL is active and FTS5 is compiled in, failing
// fast with a clear error if the driver build lacks either.
func (db *DB) verifyCapabilities() error {
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("store: reading journal_mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("store: expected WAL journal mode, got %q", mode)
	}

	// Probe FTS5 by creating a throwaway virtual table in a transaction we roll
	// back, so we never leave residue in the schema.
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: probing fts5: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("CREATE VIRTUAL TABLE _fts5_probe USING fts5(content)"); err != nil {
		return fmt.Errorf("store: FTS5 not available in this SQLite build: %w", err)
	}
	return nil
}
