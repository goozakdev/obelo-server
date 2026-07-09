package store

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate runs every embedded migration that has not yet been applied, in
// lexical filename order, inside a transaction each. It is idempotent: applied
// migrations are recorded in the schema_migrations table and skipped on
// subsequent runs, so calling Migrate on every startup is safe.
func (db *DB) Migrate() error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
		return fmt.Errorf("store: creating schema_migrations: %w", err)
	}

	applied, err := db.appliedMigrations()
	if err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: reading embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: reading migration %q: %w", name, err)
		}
		if err := db.applyMigration(version, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) appliedMigrations() (map[string]bool, error) {
	rows, err := db.Query("SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("store: reading applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scanning applied migration: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func (db *DB) applyMigration(version, body string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin migration %q: %w", version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(body); err != nil {
		return fmt.Errorf("store: applying migration %q: %w", version, err)
	}
	if _, err := tx.Exec("INSERT INTO schema_migrations (version) VALUES (?)", version); err != nil {
		return fmt.Errorf("store: recording migration %q: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %q: %w", version, err)
	}
	return nil
}
