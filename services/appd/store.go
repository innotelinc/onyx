package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// openStore opens the appd SQLite database in WAL mode
// (docs/design/04#4-config-and-state-layout). Installed apps and the containers
// the engine reported for them live here, so an app survives a restart of
// onyx-appd (and of the host) instead of vanishing from the UI.
func openStore(stateDir string) (*store, error) {
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "appd.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}
	if err := migrateStore(db); err != nil {
		db.Close()
		return nil, err
	}
	return &store{db: db}, nil
}

var storeMigrations = []string{
	// v1: installed apps — the catalog lives in code (signed manifests land with
	// the store service), what an operator installed lives here.
	`CREATE TABLE IF NOT EXISTS installed_apps (
		id           TEXT PRIMARY KEY,
		version      TEXT NOT NULL DEFAULT '',
		config       TEXT NOT NULL DEFAULT '{}',
		installed_at TEXT NOT NULL DEFAULT (datetime('now'))
	);`,
	// v2: containers the engine reported for an app. `service` is the compose
	// service name, which is the unit the lifecycle RPCs act on.
	`CREATE TABLE IF NOT EXISTS containers (
		id         TEXT PRIMARY KEY,
		app_id     TEXT NOT NULL,
		service    TEXT NOT NULL DEFAULT '',
		name       TEXT NOT NULL DEFAULT '',
		image      TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL DEFAULT 'unknown',
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);`,
	`CREATE INDEX IF NOT EXISTS containers_app_id ON containers (app_id);`,
}

func migrateStore(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for v := current; v < len(storeMigrations); v++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(storeMigrations[v]); err != nil {
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

type store struct {
	db *sql.DB
}

func (s *store) Close() error { return s.db.Close() }

// installedApp is one row of installed_apps.
type installedApp struct {
	ID          string
	Version     string
	Config      map[string]string
	InstalledAt string
}

func (s *store) markInstalled(appID, version string, config map[string]string) error {
	if config == nil {
		config = map[string]string{}
	}
	blob, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO installed_apps (id, version, config, installed_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET version = excluded.version, config = excluded.config,
		   installed_at = excluded.installed_at`,
		appID, version, string(blob))
	if err != nil {
		return fmt.Errorf("record installed app: %w", err)
	}
	return nil
}

func (s *store) markUninstalled(appID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM containers WHERE app_id = ?`, appID); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete containers: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM installed_apps WHERE id = ?`, appID); err != nil {
		tx.Rollback()
		return fmt.Errorf("delete app: %w", err)
	}
	return tx.Commit()
}

func (s *store) listInstalled() (map[string]installedApp, error) {
	rows, err := s.db.Query(`SELECT id, version, config, installed_at FROM installed_apps`)
	if err != nil {
		return nil, fmt.Errorf("list installed apps: %w", err)
	}
	defer rows.Close()
	out := map[string]installedApp{}
	for rows.Next() {
		var (
			rec  installedApp
			blob string
		)
		if err := rows.Scan(&rec.ID, &rec.Version, &blob, &rec.InstalledAt); err != nil {
			return nil, fmt.Errorf("scan installed app: %w", err)
		}
		if err := json.Unmarshal([]byte(blob), &rec.Config); err != nil {
			// A config we cannot decode is empty, not fatal: the app itself is
			// still installed and its containers are still running.
			rec.Config = map[string]string{}
		}
		out[rec.ID] = rec
	}
	return out, rows.Err()
}

// containerRecord is one containers row.
type containerRecord struct {
	ID      string
	AppID   string
	Service string
	Name    string
	Image   string
	Status  string
}

// replaceContainers swaps the recorded container set for one app: the engine's
// current view is the truth, so stale rows (a removed service) go away.
func (s *store) replaceContainers(appID string, containers []containerRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM containers WHERE app_id = ?`, appID); err != nil {
		tx.Rollback()
		return fmt.Errorf("clear containers: %w", err)
	}
	for _, c := range containers {
		if _, err := tx.Exec(
			`INSERT INTO containers (id, app_id, service, name, image, status, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, datetime('now'))
			 ON CONFLICT(id) DO UPDATE SET service = excluded.service, name = excluded.name,
			   image = excluded.image, status = excluded.status`,
			c.ID, appID, c.Service, c.Name, c.Image, c.Status); err != nil {
			tx.Rollback()
			return fmt.Errorf("record container %s: %w", c.ID, err)
		}
	}
	return tx.Commit()
}

func (s *store) setContainerStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE containers SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update container status: %w", err)
	}
	return nil
}

func (s *store) listContainers(appID string) ([]containerRecord, error) {
	query := `SELECT id, app_id, service, name, image, status FROM containers`
	args := []any{}
	if appID != "" {
		query += ` WHERE app_id = ?`
		args = append(args, appID)
	}
	query += ` ORDER BY name`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	defer rows.Close()
	var out []containerRecord
	for rows.Next() {
		var c containerRecord
		if err := rows.Scan(&c.ID, &c.AppID, &c.Service, &c.Name, &c.Image, &c.Status); err != nil {
			return nil, fmt.Errorf("scan container: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// containerByID looks up one recorded container, so a lifecycle RPC on a
// container id can find the app (project) and compose service it belongs to.
func (s *store) containerByID(id string) (containerRecord, bool, error) {
	var c containerRecord
	err := s.db.QueryRow(
		`SELECT id, app_id, service, name, image, status FROM containers WHERE id = ?`, id).
		Scan(&c.ID, &c.AppID, &c.Service, &c.Name, &c.Image, &c.Status)
	if err == sql.ErrNoRows {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("lookup container: %w", err)
	}
	return c, true, nil
}
