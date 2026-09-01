//! Pool + device registry — the SQLite database onyx-storaged owns
//! (docs/design/04#4-config-and-state-layout). Pool discovery results are
//! cached here so the data plane serves consistent answers and survives
//! restarts without re-scanning; the hotplug watcher keeps the `devices`
//! table in sync with what the kernel sees.

use std::collections::HashSet;
use std::path::Path;
use std::sync::{Arc, Mutex};

use rusqlite::{params, Connection};

use crate::onyx::{Device, DeviceEvent, Pool};

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

    // --- pools ---

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
    pub fn mark_missing(&self, seen: &HashSet<String>) -> rusqlite::Result<()> {
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

    // --- devices ---

    /// Record the current on-disk view of a block device (called every watch
    /// tick with fresh lsblk data). The stable `name` is preserved across
    /// re-observations so share identity survives relabel and replug; the
    /// `auto` reason follows the fresh scan policy, except that a device the
    /// user explicitly detached (`user_detached = 1`) stays pinned to
    /// 'manual' so the watcher never re-attaches it. `mountpoint`/`state`
    /// always follow the kernel's view.
    pub fn upsert_device(&self, d: &Device) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "INSERT INTO devices (kname, name, path, type, fs_type, label, uuid, size_bytes,
                                  mountpoint, removable, state, auto, last_seen)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, datetime('now'))
             ON CONFLICT(kname) DO UPDATE SET
               name = CASE WHEN devices.name = '' THEN excluded.name ELSE devices.name END,
               path = excluded.path,
               type = excluded.type,
               fs_type = excluded.fs_type,
               label = excluded.label,
               uuid = excluded.uuid,
               size_bytes = excluded.size_bytes,
               mountpoint = excluded.mountpoint,
               removable = excluded.removable,
               state = excluded.state,
               auto = CASE WHEN devices.user_detached = 1 THEN 'manual' ELSE excluded.auto END,
               detached_at = NULL,
               last_seen = datetime('now')",
            params![
                d.kname,
                d.name,
                d.path,
                d.r#type,
                d.fs_type,
                d.label,
                d.uuid,
                d.size_bytes as i64,
                d.mountpoint,
                b2i(d.removable),
                d.state,
                d.auto,
            ],
        )?;
        Ok(())
    }

    /// A device vanished from /sys: fetch its row, mark it detached, and
    /// return the previous row so the caller can unmount before the record
    /// forgets the mountpoint. Returns None if the device was unknown.
    pub fn mark_detached(&self, kname: &str) -> rusqlite::Result<Option<Device>> {
        let conn = self.conn.lock().unwrap();
        let prev = get_device_by(&*conn, "kname = ?1", params![kname])?;
        if prev.is_some() {
            conn.execute(
                "UPDATE devices SET state = 'detached', mountpoint = '', detached_at = datetime('now')
                 WHERE kname = ?1",
                params![kname],
            )?;
        }
        Ok(prev)
    }

    /// Note that a device was mounted at `mountpoint` (called after the
    /// privileged mount succeeded).
    pub fn set_mounted(&self, kname: &str, mountpoint: &str) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE devices SET state = 'mounted', mountpoint = ?2, detached_at = NULL WHERE kname = ?1",
            params![kname, mountpoint],
        )?;
        Ok(())
    }

    /// Note that a device was unmounted (called after `umount`).
    pub fn set_unmounted(&self, kname: &str) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE devices SET state = 'attached', mountpoint = '' WHERE kname = ?1",
            params![kname],
        )?;
        Ok(())
    }

    /// True when the user explicitly detached this device (`onyx device
    /// detach`): it must not auto-attach again until an explicit re-attach.
    pub fn is_user_detached(&self, kname: &str) -> rusqlite::Result<bool> {
        let conn = self.conn.lock().unwrap();
        conn.query_row(
            "SELECT user_detached FROM devices WHERE kname = ?1",
            params![kname],
            |row| row.get::<_, i64>(0).map(|v| v != 0),
        )
        .or_else(|e| match e {
            rusqlite::Error::QueryReturnedNoRows => Ok(false),
            other => Err(other),
        })
    }

    /// Pin a device out of auto-attach (`onyx device detach` while the drive
    /// is still plugged in). The next scan upsert keeps `auto = 'manual'`.
    pub fn mark_detached_by_user(&self, kname: &str) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE devices SET user_detached = 1, auto = 'manual' WHERE kname = ?1",
            params![kname],
        )?;
        Ok(())
    }

    /// Reverse `mark_detached_by_user` and restore the device's auto-attach
    /// policy reason (`onyx device attach`).
    pub fn mark_attached_by_user(&self, kname: &str, auto: &str) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE devices SET user_detached = 0, auto = ?2 WHERE kname = ?1",
            params![kname, auto],
        )?;
        Ok(())
    }

    /// All known devices, ordered by name. Detached entries remain until
    /// pruned so a removal shows up in the UI.
    pub fn list_devices(&self) -> rusqlite::Result<Vec<Device>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare(
            "SELECT kname, name, path, type, fs_type, label, uuid, size_bytes,
                    mountpoint, removable, state, auto, health_status, temperature_c
             FROM devices ORDER BY name",
        )?;
        let rows = stmt.query_map([], row_to_device)?;
        rows.collect()
    }

    /// Find one device by share name or kernel name (whichever the caller had).
    pub fn get_device(&self, name_or_kname: &str) -> rusqlite::Result<Option<Device>> {
        let conn = self.conn.lock().unwrap();
        get_device_by(&*conn, "name = ?1 OR kname = ?1", params![name_or_kname])
    }

    /// All device kname -> stable-name pairs, for uniqueness checks.
    pub fn device_names(&self) -> rusqlite::Result<Vec<(String, String)>> {
        let conn = self.conn.lock().unwrap();
        let mut stmt = conn.prepare("SELECT kname, name FROM devices WHERE state != 'detached'")?;
        let rows = stmt.query_map([], |row| Ok((row.get(0)?, row.get(1)?)))?;
        rows.collect()
    }

    /// Forget detached devices that vanished more than `older_than_minutes`
    /// ago so the table cannot grow forever across repeated plugs.
    pub fn prune_detached(&self, older_than_minutes: i64) -> rusqlite::Result<usize> {
        let conn = self.conn.lock().unwrap();
        let n = conn.execute(
            "DELETE FROM devices WHERE state = 'detached'
             AND detached_at < datetime('now', '-' || ?1 || ' minutes')",
            params![older_than_minutes],
        )?;
        Ok(n)
    }

    /// Record the latest SMART health result for a device.
    pub fn set_health(&self, kname: &str, status: &str, temp_c: Option<u32>) -> rusqlite::Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE devices SET health_status = ?2, temperature_c = ?3,
                    health_checked_at = datetime('now')
             WHERE kname = ?1",
            params![kname, status, temp_c.map(i64::from).unwrap_or(0)],
        )?;
        Ok(())
    }

    // --- device event audit trail ---

    /// Append one audit event; returns the stored event with its assigned id
    /// and timestamp filled in (for the live broadcast to mirror the log).
    pub fn push_event(&self, e: &DeviceEvent) -> rusqlite::Result<DeviceEvent> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "INSERT INTO device_events (kname, name, event, detail) VALUES (?1, ?2, ?3, ?4)",
            params![e.kname, e.name, e.event, e.detail],
        )?;
        let id = conn.last_insert_rowid() as u64;
        let ts: String = conn.query_row("SELECT datetime('now')", [], |r| r.get(0))?;
        Ok(DeviceEvent { id, ts, ..e.clone() })
    }

    /// Page the audit trail. With `after_id > 0`: events with id > after_id,
    /// oldest-first (resume point for clients); otherwise the most recent
    /// `limit` events, newest-first.
    pub fn list_events(
        &self,
        limit: u32,
        after_id: u64,
        kname: &str,
    ) -> rusqlite::Result<Vec<DeviceEvent>> {
        let limit = if limit == 0 { 100 } else { limit as i64 };
        let conn = self.conn.lock().unwrap();
        let rows = if after_id > 0 && kname.is_empty() {
            conn.prepare("SELECT id, ts, kname, name, event, detail FROM device_events WHERE id > ?1 ORDER BY id ASC LIMIT ?2")?
                .query_map(params![after_id as i64, limit], row_to_event)?
                .collect::<Result<Vec<_>, _>>()?
        } else if after_id > 0 {
            conn.prepare("SELECT id, ts, kname, name, event, detail FROM device_events WHERE id > ?1 AND kname = ?2 ORDER BY id ASC LIMIT ?3")?
                .query_map(params![after_id as i64, kname, limit], row_to_event)?
                .collect::<Result<Vec<_>, _>>()?
        } else if kname.is_empty() {
            conn.prepare("SELECT id, ts, kname, name, event, detail FROM device_events ORDER BY id DESC LIMIT ?1")?
                .query_map(params![limit], row_to_event)?
                .collect::<Result<Vec<_>, _>>()?
        } else {
            conn.prepare("SELECT id, ts, kname, name, event, detail FROM device_events WHERE kname = ?1 ORDER BY id DESC LIMIT ?2")?
                .query_map(params![kname, limit], row_to_event)?
                .collect::<Result<Vec<_>, _>>()?
        };
        Ok(rows)
    }
}

fn row_to_event(row: &rusqlite::Row) -> rusqlite::Result<DeviceEvent> {
    Ok(DeviceEvent {
        id: row.get::<_, i64>(0)? as u64,
        ts: row.get(1)?,
        kname: row.get(2)?,
        name: row.get(3)?,
        event: row.get(4)?,
        detail: row.get(5)?,
    })
}

fn get_device_by(
    conn: &Connection,
    where_clause: &str,
    args: &[&dyn rusqlite::ToSql],
) -> rusqlite::Result<Option<Device>> {
    let sql = format!(
        "SELECT kname, name, path, type, fs_type, label, uuid, size_bytes,
                mountpoint, removable, state, auto, health_status, temperature_c
         FROM devices WHERE {where_clause} LIMIT 1"
    );
    let mut stmt = conn.prepare(&sql)?;
    let mut rows = stmt.query(args)?;
    match rows.next()? {
        Some(row) => Ok(Some(row_to_device(row)?)),
        None => Ok(None),
    }
}

fn row_to_device(row: &rusqlite::Row) -> rusqlite::Result<Device> {
    Ok(Device {
        kname: row.get(0)?,
        name: row.get(1)?,
        path: row.get(2)?,
        r#type: row.get(3)?,
        fs_type: row.get(4)?,
        label: row.get(5)?,
        uuid: row.get(6)?,
        size_bytes: row.get::<_, i64>(7)? as u64,
        mountpoint: row.get(8)?,
        removable: row.get::<_, i64>(9)? != 0,
        state: row.get(10)?,
        auto: row.get(11)?,
        health_status: row.get(12)?,
        temperature_c: row.get::<_, i64>(13)? as u32,
    })
}

fn b2i(b: bool) -> i64 {
    if b {
        1
    } else {
        0
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
         );
         CREATE TABLE IF NOT EXISTS devices (
           kname        TEXT PRIMARY KEY,
           name         TEXT NOT NULL,
           path         TEXT NOT NULL,
           type         TEXT NOT NULL DEFAULT '',
           fs_type      TEXT NOT NULL DEFAULT '',
           label        TEXT NOT NULL DEFAULT '',
           uuid         TEXT NOT NULL DEFAULT '',
           size_bytes   INTEGER NOT NULL DEFAULT 0,
           mountpoint   TEXT NOT NULL DEFAULT '',
           removable    INTEGER NOT NULL DEFAULT 0,
           state        TEXT NOT NULL DEFAULT 'attached',
           auto         TEXT NOT NULL DEFAULT 'manual',
           user_detached INTEGER NOT NULL DEFAULT 0,
           detached_at  TEXT,
           last_seen    TEXT NOT NULL DEFAULT (datetime('now')),
           health_status TEXT NOT NULL DEFAULT 'unknown',
           temperature_c INTEGER NOT NULL DEFAULT 0,
           health_checked_at TEXT
         );
         CREATE TABLE IF NOT EXISTS device_events (
           id     INTEGER PRIMARY KEY AUTOINCREMENT,
           ts     TEXT NOT NULL DEFAULT (datetime('now')),
           kname  TEXT NOT NULL,
           name   TEXT NOT NULL DEFAULT '',
           event  TEXT NOT NULL,
           detail TEXT NOT NULL DEFAULT ''
         );",
    )?;
    // Older registries created the devices table before health columns
    // existed; add them if missing so on-disk DBs migrate in place.
    ensure_column(conn, "devices", "health_status", "TEXT NOT NULL DEFAULT 'unknown'")?;
    ensure_column(conn, "devices", "temperature_c", "INTEGER NOT NULL DEFAULT 0")?;
    ensure_column(conn, "devices", "user_detached", "INTEGER NOT NULL DEFAULT 0")?;
    Ok(())
}

/// ALTER TABLE ADD COLUMN when the column is not already present.
fn ensure_column(conn: &Connection, table: &str, column: &str, ddl: &str) -> rusqlite::Result<()> {
    let exists: bool = conn.query_row(
        &format!("SELECT COUNT(*) FROM pragma_table_info('{table}') WHERE name = ?1"),
        params![column],
        |row| Ok(row.get::<_, i64>(0)? > 0),
    )?;
    if !exists {
        conn.execute(&format!("ALTER TABLE {table} ADD COLUMN {column} {ddl}"), [])?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A removable USB partition as the watcher would store it on first
    /// observation (`--auto-attach=removable` -> auto = "removable").
    fn usb_device() -> Device {
        Device {
            kname: "sdz1".to_string(),
            name: "usb-data".to_string(),
            path: "/dev/sdz1".to_string(),
            r#type: "part".to_string(),
            fs_type: "vfat".to_string(),
            label: "USB DATA".to_string(),
            uuid: "ABCD-1234".to_string(),
            size_bytes: 32_000_000_000,
            mountpoint: String::new(),
            removable: true,
            state: "attached".to_string(),
            auto: "removable".to_string(),
            health_status: String::new(),
            temperature_c: 0,
        }
    }

    #[test]
    fn user_detach_sticks_across_scan_upserts() {
        let dir = std::env::temp_dir().join(format!("onyx-registry-ut-{}", std::process::id()));
        let reg = Registry::open(&dir).expect("open registry");

        // First observation: removable drive, eligible under the policy.
        reg.upsert_device(&usb_device()).unwrap();
        assert!(!reg.is_user_detached("sdz1").unwrap());
        assert_eq!(reg.get_device("sdz1").unwrap().unwrap().auto, "removable");

        // `onyx device detach` pins the device out of auto-attach.
        reg.mark_detached_by_user("sdz1").unwrap();
        assert!(reg.is_user_detached("sdz1").unwrap());
        assert_eq!(reg.get_device("sdz1").unwrap().unwrap().auto, "manual");

        // The watcher keeps re-observing the still-plugged-in drive with a
        // fresh policy value every tick — the pin must survive those upserts.
        let fresh = usb_device(); // auto = "removable" again
        for _ in 0..3 {
            reg.upsert_device(&fresh).unwrap();
        }
        assert!(reg.is_user_detached("sdz1").unwrap());
        assert_eq!(reg.get_device("sdz1").unwrap().unwrap().auto, "manual");

        // Unplug + replug re-observations keep the pin as well.
        let replugged = usb_device();
        reg.upsert_device(&replugged).unwrap();
        assert!(reg.is_user_detached("sdz1").unwrap());
        assert_eq!(reg.get_device("sdz1").unwrap().unwrap().auto, "manual");

        // `onyx device attach` clears the opt-out and restores the policy.
        reg.mark_attached_by_user("sdz1", "removable").unwrap();
        reg.upsert_device(&fresh).unwrap();
        assert!(!reg.is_user_detached("sdz1").unwrap());
        assert_eq!(reg.get_device("sdz1").unwrap().unwrap().auto, "removable");

        std::fs::remove_dir_all(&dir).ok();
    }
}