package main

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// openStore opens the vmm SQLite database in WAL mode
// (docs/design/04#4-config-and-state-layout). The VM inventory lives here so a
// restart of onyx-vmm (or of the host) does not lose the machines, their disk
// images or their last known state.
func openStore(stateDir string) (*vmStore, error) {
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "vmm.sqlite"))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure database: %w", err)
	}
	if err := migrateStore(db); err != nil {
		db.Close()
		return nil, err
	}
	return &vmStore{db: db}, nil
}

var storeMigrations = []string{
	`CREATE TABLE IF NOT EXISTS vms (
		id         TEXT PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		status     TEXT NOT NULL DEFAULT 'stopped',
		vcpus      INTEGER NOT NULL DEFAULT 1,
		memory_mb  INTEGER NOT NULL DEFAULT 512,
		disk_mb    INTEGER NOT NULL DEFAULT 0,
		disk       TEXT NOT NULL DEFAULT '',
		os         TEXT NOT NULL DEFAULT '',
		iso        TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);`,
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

type vmStore struct {
	db *sql.DB
}

func (s *vmStore) Close() error { return s.db.Close() }

// vmRecord is one row of vms.
type vmRecord struct {
	ID        string
	Name      string
	Status    string
	VCPUs     int32
	MemoryMB  int64
	DiskMB    int64
	Disk      string
	OS        string
	ISO       string
	CreatedAt string
}

const vmColumns = `id, name, status, vcpus, memory_mb, disk_mb, disk, os, iso, created_at`

func scanVM(row interface{ Scan(...any) error }) (vmRecord, error) {
	var rec vmRecord
	err := row.Scan(&rec.ID, &rec.Name, &rec.Status, &rec.VCPUs, &rec.MemoryMB,
		&rec.DiskMB, &rec.Disk, &rec.OS, &rec.ISO, &rec.CreatedAt)
	return rec, err
}

func (s *vmStore) insert(rec vmRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO vms (`+vmColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		rec.ID, rec.Name, rec.Status, rec.VCPUs, rec.MemoryMB, rec.DiskMB, rec.Disk, rec.OS, rec.ISO)
	if err != nil {
		return fmt.Errorf("insert vm: %w", err)
	}
	return nil
}

func (s *vmStore) updateStatus(id, status string) error {
	if _, err := s.db.Exec(`UPDATE vms SET status = ? WHERE id = ?`, status, id); err != nil {
		return fmt.Errorf("update vm status: %w", err)
	}
	return nil
}

func (s *vmStore) delete(id string) error {
	if _, err := s.db.Exec(`DELETE FROM vms WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete vm: %w", err)
	}
	return nil
}

func (s *vmStore) list() ([]vmRecord, error) {
	rows, err := s.db.Query(`SELECT ` + vmColumns + ` FROM vms ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	defer rows.Close()
	var out []vmRecord
	for rows.Next() {
		rec, err := scanVM(rows)
		if err != nil {
			return nil, fmt.Errorf("scan vm: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// byID looks a VM up by its id or its name, so both the API's `{id}` and a CLI's
// name work without the caller guessing which it has.
func (s *vmStore) byID(idOrName string) (vmRecord, bool, error) {
	if idOrName == "" {
		return vmRecord{}, false, nil
	}
	row := s.db.QueryRow(`SELECT `+vmColumns+` FROM vms WHERE id = ? OR name = ?`, idOrName, idOrName)
	rec, err := scanVM(row)
	if err == sql.ErrNoRows {
		return vmRecord{}, false, nil
	}
	if err != nil {
		return vmRecord{}, false, fmt.Errorf("lookup vm: %w", err)
	}
	return rec, true, nil
}

func (s *vmStore) nameTaken(name string) (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM vms WHERE name = ?`, name).Scan(&count); err != nil {
		return false, fmt.Errorf("check vm name: %w", err)
	}
	return count > 0, nil
}

// nextID returns the next sequential VM id. Sequential ids keep the CLI and the
// UI readable ("vm-1") and make the id stable across restarts.
func (s *vmStore) nextID() (string, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM vms`).Scan(&count); err != nil {
		return "", fmt.Errorf("count vms: %w", err)
	}
	for i := count + 1; ; i++ {
		id := fmt.Sprintf("vm-%d", i)
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM vms WHERE id = ?`, id).Scan(&exists); err != nil {
			return "", fmt.Errorf("check vm id: %w", err)
		}
		if exists == 0 {
			return id, nil
		}
	}
}
