package main

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// openDB opens the per-service SQLite database in WAL mode
// (docs/design/04#4-config-and-state-layout).
func openDB(stateDir string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "core.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrations is the ordered schema history; index i applies at version i+1.
var migrations = []string{
	// v1: service registry — who is registered, at what version, in what state.
	`CREATE TABLE IF NOT EXISTS service_registry (
		name       TEXT PRIMARY KEY,
		version    TEXT NOT NULL,
		status     TEXT NOT NULL DEFAULT 'unknown',
		updated_at TEXT NOT NULL DEFAULT (datetime('now'))
	);`,
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for v := current; v < len(migrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, v+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", v+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", v+1, err)
		}
	}
	return nil
}