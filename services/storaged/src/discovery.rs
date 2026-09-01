//! Pool discovery (docs/design/05#7-disk-management, 04#1).
//!
//! onyx-storaged never touches the filesystem directly: enumeration runs
//! `btrfs filesystem show --raw` *inside onyx-privd* (the one root process),
//! and the parsed results are cached in the SQLite registry.

use std::collections::HashSet;
use std::sync::Arc;

use tonic::transport::Channel;

use crate::onyx::privd_client::PrivdClient;
use crate::onyx::{Pool, PrivOp, PrivRequest};
use crate::registry::Registry;

/// A pool as reported by `btrfs filesystem show --raw`.
#[derive(Debug, PartialEq, Eq)]
pub struct ParsedPool {
    /// Btrfs label; "none" when unlabeled.
    pub label: String,
    pub uuid: String,
    pub total_bytes: u64,
    pub used_bytes: u64,
}

/// Refresh the registry from `btrfs filesystem show --raw`, run via onyx-privd.
/// Returns the full set of pools now known (including offline ones remaining in
/// the registry from previous scans).
pub async fn refresh_pools(
    privd: &mut PrivdClient<Channel>,
    registry: &Arc<Registry>,
) -> Result<Vec<Pool>, String> {	let resp = privd
		.run(PrivRequest {
			op: PrivOp::BtrfsFilesystemShowRaw as i32,
			args: Vec::new(),
		})
		.await
		.map_err(|e| format!("privd: {e}"))?
		.into_inner();

	if resp.exit_code != 0 {
		let stderr = String::from_utf8_lossy(&resp.stderr);
		let detail = if stderr.trim().is_empty() {
			String::new()
		} else {
			format!(": {}", stderr.trim())
		};
		return Err(format!("btrfs filesystem show exited {}{}", resp.exit_code, detail));
	}

	let stdout = String::from_utf8_lossy(&resp.stdout);
    let parsed = parse_show_raw(&stdout);

    let mut seen = HashSet::new();
    for p in &parsed {
        seen.insert(p.uuid.clone());
        let pool = to_pool(p);
        registry
            .upsert_pool(&pool)
            .map_err(|e| format!("registry upsert: {e}"))?;
    }
    registry
        .mark_missing(&seen)
        .map_err(|e| format!("registry mark_missing: {e}"))?;

    let all = registry.list_pools().map_err(|e| format!("registry list: {e}"))?;
    tracing::info!(found = parsed.len(), known = all.len(), "pool refresh complete");
    Ok(all)
}

fn to_pool(p: &ParsedPool) -> Pool {
    // Unlabeled filesystems carry the uuid as their display name.
    let name = if p.label.is_empty() || p.label == "none" { p.uuid.clone() } else { p.label.clone() };
    Pool {
        name,
        uuid: p.uuid.clone(),
        fs_type: "btrfs".into(),
        total_bytes: p.total_bytes,
        used_bytes: p.used_bytes,
        state: "online".into(),
    }
}

/// Parse the output of `btrfs filesystem show --raw`.
///
/// Format per filesystem (label line + optional stats/device lines):
///
/// ```text
/// Label: 'pool1'  uuid: 96ee0259-...  OR  Label: none  uuid: ...
///     Total devices 2 FS bytes used 524288000
///     devid    1 size 10737418240 used 5368709120 path /dev/sda1
/// ```
///
/// `--raw` guarantees bare byte counts (no units). Device `size` lines sum to
/// the pool capacity; `FS bytes used` is pool-level usage.
pub fn parse_show_raw(output: &str) -> Vec<ParsedPool> {
    let mut pools: Vec<ParsedPool> = Vec::new();
    let mut current: Option<ParsedPool> = None;

    for raw_line in output.lines() {
        let line = raw_line.trim();
        if let Some(rest) = line.strip_prefix("Label:") {
            // Commit the previous filesystem, start a new one.
            if let Some(prev) = current.take() {
                pools.push(prev);
            }
            let (label, uuid) = parse_label_line(rest);
            current = Some(ParsedPool { label, uuid, total_bytes: 0, used_bytes: 0 });
            continue;
        }
        let Some(pool) = current.as_mut() else { continue };

        if let Some(idx) = line.find("FS bytes used") {
            if let Some(n) = after_u64(&line[idx + "FS bytes used".len()..]) {
                pool.used_bytes = n;
            }
        }
        if let Some(rest) = line.strip_prefix("devid") {
            // devid N size S used U path P
            for (k, v) in tokens_after(rest) {
                if k == "size" {
                    if let Ok(n) = v.parse::<u64>() {
                        pool.total_bytes += n;
                    }
                }
            }
        }
    }
    if let Some(prev) = current.take() {
        pools.push(prev);
    }
    pools
}

/// Parse the remainder of a `Label: ...` line into (label, uuid).
/// Accepts `'name'  uuid: <uuid>` and `none  uuid: <uuid>`.
fn parse_label_line(rest: &str) -> (String, String) {
    let mut label = String::new();
    let mut uuid = String::new();
    let trimmed = rest.trim();
    if let Some(q) = trimmed.strip_prefix('\'') {
        if let Some(end) = q.find('\'') {
            label = q[..end].to_string();
        }
    } else if let Some(end) = trimmed.find("uuid:") {
        // unquoted (e.g. "none") — label is whatever precedes "uuid:"
        label = trimmed[..end].trim().to_string();
    }
    if let Some(u) = trimmed.find("uuid:") {
        uuid = trimmed[u + "uuid:".len()..].trim().to_string();
    }
    (label, uuid)
}

/// Parse `key value key value ...` pairs from a tokens string, skipping a
/// leading index token if present.
fn tokens_after(s: &str) -> Vec<(String, String)> {
    let mut out = Vec::new();
    let mut it = s.split_whitespace();
    // devid lines start with a number (the devid); skip it.
    let _ = it.next();
    let mut key: Option<String> = None;
    for tok in it {
        match key.take() {
            None => key = Some(tok.to_string()),
            Some(k) => {
                out.push((k, tok.to_string()));
            }
        }
    }
    out
}

/// Read the first u64 from a token string (e.g. `" 524288000"`), tolerating a
/// trailing unit token just in case --raw was not honored.
fn after_u64(s: &str) -> Option<u64> {
    s.split_whitespace().next().and_then(|t| t.parse().ok())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_single_pool() {
        let out = "Label: 'pool1'  uuid: 96ee0259-1111-4a1a-8a8a-111111111111\n\
                    \tTotal devices 2 FS bytes used 524288000\n\
                    \tdevid    1 size 10737418240 used 5368709120 path /dev/sda1\n\
                    \tdevid    2 size 10737418240 used 5368709120 path /dev/sdb1\n";
        let pools = parse_show_raw(out);
        assert_eq!(pools.len(), 1);
        assert_eq!(pools[0].label, "pool1");
        assert_eq!(pools[0].uuid, "96ee0259-1111-4a1a-8a8a-111111111111");
        assert_eq!(pools[0].total_bytes, 2 * 10737418240);
        assert_eq!(pools[0].used_bytes, 524288000);
    }

    #[test]
    fn parses_unlabeled_and_multiple_pools() {
        let out = "Label: 'pool1'  uuid: 96ee0259-1111-4a1a-8a8a-111111111111\n\
                    \tTotal devices 1 FS bytes used 4096\n\
                    \tdevid    1 size 10737418240 used 4096 path /dev/sda1\n\
                    \n\
                    Label: none  uuid: aabbccdd-2222-4b2b-9b9b-222222222222\n\
                    \tTotal devices 1 FS bytes used 8192\n\
                    \tdevid    1 size 5368709120 used 8192 path /dev/nvme0n1\n";
        let pools = parse_show_raw(out);
        assert_eq!(pools.len(), 2);
        assert_eq!(pools[0].label, "pool1");
        assert_eq!(pools[1].label, "none");
        assert_eq!(pools[1].uuid, "aabbccdd-2222-4b2b-9b9b-222222222222");
    }

    #[test]
    fn empty_output_yields_no_pools() {
        assert!(parse_show_raw("").is_empty());
        assert!(parse_show_raw("ERROR: no btrfs filesystems found\n").is_empty());
    }
}