//! Pool registry — the SQLite database onyx-storaged owns
//! (docs/design/04#4-config-and-state-layout). Discovery results are cached
//! here so the data plane serves consistent answers and survives restarts
//! without re-scanning.

use std::path::Path;
use std::sync::{Arc, Mutex};

use rusqlite::{params, Connection};

use crate::onyx::Pool;

pub struct Registry {
    conn: Mutex<Connection>,
}

impl Registry {
    /// Open (creating if needed) the registry DB in WAL mode at
    /// `<state_dir>/onyx-storaged.sqlite`.
    pub fn open(state_dir: &Path) -> Result<Arc<Self>, rusqlite::Error> {
        std::fs::create_dir_all(state_dir)
            .map_err(|e| rusqlite::Error::ToSqlConversionFailure(Box::new(e)))?;
        let conn = Connection::open(state_dir.join("onyx-storaged.sqlite"))?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "synchronous", "NORMAL")?;
        migrate(&conn)?;
        Ok(Arc::new(Registry { conn: Mutex::new(conn) }))
    }

    /// Upsert a discovered pool. Total/used are refresh fields; everything else
    /// is keyed on the Btrfs uuid (stable across renames).
    pub fn upsert_pool(&self, p: &Pool) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "INSERT INTO pools (uuid, name, fs_type, total_bytes, used_bytes, state, discovered_at)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, datetime('now'))
             ON CONFLICT(uuid) DO UPDATE SET
               name = excluded.name,
               fs_type = excluded.fs_type,
               total_bytes = excluded.total_bytes,
               used_bytes = excluded.used_bytes,
               state = excluded.state,
               discovered_at = datetime('now')",
            params![p.uuid, p.name, p.fs_type, p.total_bytes as i64, p.used_bytes as i64, p.state],
        )?;
        Ok(())
    }

    /// Mark pools that vanished from the last scan as offline (they remain
    /// listed so the UI can show a degraded pool instead of a silent hole).
    pub fn mark_missing(&self, seen: &std::collections::HashSet<String>) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare("SELECT uuid FROM pools WHERE state != 'offline'")?;
        let known: Vec<String> = stmt.query_map([], |row| row.get(0))?.collect::<Result<_, _>>()?;
        for uuid in known {
            if !seen.contains(&uuid) {
                conn.execute("UPDATE pools SET state = 'offline' WHERE uuid = ?1", params![uuid])?;
            }
        }
        Ok(())
    }

    /// All known pools, ordered by name.
    pub fn list_pools(&self) -> rusqlite::Result<Vec<Pool>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare(
            "SELECT uuid, name, fs_type, total_bytes, used_bytes, state FROM pools ORDER BY name",
        )?;
        let rows = stmt.query_map([], |row| {
            Ok(Pool {
                uuid: row.get(0)?,
                name: row.get(1)?,
                fs_type: row.get(2)?,
                total_bytes: row.get::<_, i64>(3)? as u64,
                used_bytes: row.get::<_, i64>(4)? as u64,
                state: row.get(5)?,
            })
        })?;
        rows.collect()
    }
}

fn migrate(conn: &Connection) -> rusqlite::Result<()> {
    conn.execute_batch(
        "CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);
         CREATE TABLE IF NOT EXISTS pools (
           uuid          TEXT PRIMARY KEY,
           name          TEXT NOT NULL,
           fs_type       TEXT NOT NULL DEFAULT 'btrfs',
           total_bytes   INTEGER NOT NULL DEFAULT 0,
           used_bytes    INTEGER NOT NULL DEFAULT 0,
           state         TEXT NOT NULL DEFAULT 'unknown',
           discovered_at TEXT NOT NULL DEFAULT (datetime('now'))
         );",
    )?;
    Ok(())
}