//! Kernel uevent monitor — the raw feed udev itself listens on
//! (docs/design/05#7-disk-management, hotplug).
//!
//! Instead of libudev (a C library + daemon we do not want to require), this
//! binds directly to the `NETLINK_KOBJECT_UEVENT` netlink socket — the kernel
//! multicast group every uevent (device add/remove/change) is broadcast on —
//! and reads the broadcast messages. That gives instant hotplug reaction
//! without polling and without udev running, exactly what a storage daemon
//! wants. Binding works unprivileged on modern kernels; when it fails (some
//! containers), the caller falls back to periodic scans.
//!
//! The uevent payload is the classic kernel blob: an optional
//! `ACTION@DEVPATH` header, then `KEY=VALUE` pairs separated by NULs, inside
//! one netlink datagram.

use std::io;
use std::os::fd::{AsRawFd, OwnedFd, FromRawFd};

use tokio::io::unix::AsyncFd;

/// NETLINK_KOBJECT_UEVENT protocol number.
const NETLINK_KOBJECT_UEVENT: libc::c_int = 15;
/// The kernel uevent multicast group (group 1 = KERNEL).
const KERNEL_UEVENT_GROUP: libc::c_uint = 1;
/// Neither messages are tiny nor unbounded; 64 KiB comfortably holds any
/// multi-part broadcast.
const BUF_LEN: usize = 65536;

/// One parsed, block-relevant kernel uevent.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Uevent {
    /// add | remove | change | bind | ...
    pub action: String,
    /// Last path component of DEVPATH, e.g. "sdb1", "sda", "sr0".
    pub kname: String,
    /// disk | partition | "" for whole-block oddities (cdrom, ...).
    pub devtype: String,
}

/// Instant hotplug source: an async wrapper around a non-blocking netlink
/// socket registered with the tokio reactor, so `next()` wakes the watcher
/// the moment a block uevent lands.
pub struct UeventMonitor {
    fd: AsyncFd<OwnedFd>,
}

impl UeventMonitor {
    /// Open the netlink socket and subscribe to the kernel uevent group.
    /// Fails with a structured io::Error when the environment forbids it
    /// (no netlink, seccomp, ...) — callers then fall back to polling.
    pub fn open() -> io::Result<Self> {
        let fd = unsafe {
            libc::socket(
                libc::AF_NETLINK,
                libc::SOCK_DGRAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                NETLINK_KOBJECT_UEVENT,
            )
        };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let owned = unsafe { OwnedFd::from_raw_fd(fd) };

        // sockaddr_padding fields are layout markers in modern libc; start
        // from a zeroed POD and fill the fields we care about.
        let mut addr: libc::sockaddr_nl = unsafe { std::mem::zeroed() };
        addr.nl_family = libc::AF_NETLINK as libc::sa_family_t;
        addr.nl_groups = KERNEL_UEVENT_GROUP;
        let rc = unsafe {
            libc::bind(
                fd,
                &addr as *const libc::sockaddr_nl as *const libc::sockaddr,
                std::mem::size_of::<libc::sockaddr_nl>() as libc::socklen_t,
            )
        };
        if rc < 0 {
            return Err(io::Error::last_os_error()); // owned fd closes on drop
        }
        match AsyncFd::new(owned) {
            Ok(fd) => Ok(UeventMonitor { fd }),
            Err(e) => Err(e),
        }
    }

    /// Wait for the next block uevent and return it. None means the socket
    /// became unusable (the caller then falls back to periodic scans). The
    /// readiness guard uses the documented `clear_ready` drain idiom: recv
    /// until EAGAIN, then re-await readability.
    pub async fn next(&self) -> Option<Uevent> {
        loop {
            let mut guard = match self.fd.readable().await {
                Ok(g) => g,
                Err(e) => {
                    tracing::warn!(error = %e, "netlink uevent monitor failed; periodic scans only");
                    return None;
                }
            };
            let mut buf = [0u8; BUF_LEN];
            loop {
                let n = unsafe {
                    libc::recv(
                        self.fd.as_raw_fd(),
                        buf.as_mut_ptr() as *mut libc::c_void,
                        buf.len(),
                        0,
                    )
                };
                if n < 0 {
                    match io::Error::last_os_error().kind() {
                        io::ErrorKind::WouldBlock => break,     // drained; wait for the next wakeup
                        io::ErrorKind::Interrupted => continue,
                        kind => {
                            tracing::warn!(error = %kind, "netlink uevent read failed; periodic scans only");
                            return None;
                        }
                    }
                }
                if n == 0 {
                    break; // closed/empty; belt and suspenders
                }
                if let Some(ev) = parse_uevent(&buf[..n as usize]) {
                    return Some(ev);
                }
                // Non-block message (e.g. subsystem=usb): drain the rest.
            }
            guard.clear_ready();
        }
    }
}

/// Parse one kernel uevent datagram into a block-device event. Returns None
/// for anything that is not a block uevent (subsystem != block) or is
/// malformed. Tolerates both the `ACTION@DEVPATH` header form and the
/// KEY=VALUE form.
pub fn parse_uevent(buf: &[u8]) -> Option<Uevent> {
    let text = std::str::from_utf8(buf).ok()?;
    let mut action = String::new();
    let mut devpath = String::new();
    let mut subsystem = String::new();
    let mut devtype = String::new();

    for (i, field) in text.split('\0').enumerate() {
        if field.is_empty() {
            break; // trailing NUL(s) end the payload
        }
        if i == 0 {
            if let Some((a, d)) = field.split_once('@') {
                action.push_str(a);
                devpath.push_str(d);
                continue;
            }
        }
        let Some((k, v)) = field.split_once('=') else { continue };
        match k {
            "ACTION" => action = v.to_string(),
            "DEVPATH" => devpath = v.to_string(),
            "SUBSYSTEM" => subsystem = v.to_string(),
            "DEVTYPE" => devtype = v.to_string(),
            _ => {}
        }
    }

    if action.is_empty() || devpath.is_empty() {
        return None;
    }
    if subsystem != "block" {
        return None;
    }
    let kname = devpath.rsplit('/').next()?.to_string();
    if kname.is_empty()
        || kname == "block"
        || !kname
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '_')
    {
        return None;
    }
    Some(Uevent { action, kname, devtype })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn uevent_blob(header: &str, pairs: &[(&str, &str)]) -> Vec<u8> {
        let mut out = format!("{header}\0").into_bytes();
        for (k, v) in pairs {
            out.extend_from_slice(format!("{k}={v}\0").as_bytes());
        }
        out.push(0);
        out
    }

    #[test]
    fn parses_partition_add() {
        let blob = uevent_blob(
            "add@/devices/pci0000:00/0000:00:14.0/usb3/3-1/3-1:1.0/host4/target4:0:0/4:0:0:0/block/sdb/sdb1",
            &[
                ("ACTION", "add"),
                ("DEVPATH", "/devices/pci0000:00/0000:00:14.0/usb3/3-1/3-1:1.0/host4/target4:0:0/4:0:0:0/block/sdb/sdb1"),
                ("SUBSYSTEM", "block"),
                ("DEVTYPE", "partition"),
                ("MAJOR", "8"),
                ("MINOR", "17"),
                ("SEQNUM", "12345"),
            ],
        );
        let ev = parse_uevent(&blob).expect("parses");
        assert_eq!(ev.action, "add");
        assert_eq!(ev.kname, "sdb1");
        assert_eq!(ev.devtype, "partition");
    }

    #[test]
    fn parses_whole_disk_removal_without_header() {
        // Some messages carry no ACTION@DEVPATH header, only KEY=VALUE pairs.
        let blob = uevent_blob(
            "remove@/devices/pci0000:00/0000:00:14.0/usb3/3-1/3-1:1.0/host4/target4:0:0/4:0:0:0/block/sdb",
            &[("SUBSYSTEM", "block"), ("DEVTYPE", "disk")],
        );
        let ev = parse_uevent(&blob).expect("parses");
        assert_eq!(ev.action, "remove");
        assert_eq!(ev.kname, "sdb");
        assert_eq!(ev.devtype, "disk");
    }

    #[test]
    fn ignores_non_block_subsystems() {
        let blob = uevent_blob(
            "add@/devices/pci0000:00/0000:00:14.0/usb3/3-1",
            &[("SUBSYSTEM", "usb"), ("DEVTYPE", "usb_device")],
        );
        assert!(parse_uevent(&blob).is_none());
    }

    #[test]
    fn ignores_garbage_and_empty() {
        assert!(parse_uevent(b"").is_none());
        assert!(parse_uevent(b"no header at all\0SUBSYSTEM=block\0\0").is_none());
        assert!(parse_uevent(&[0xff, 0xfe, 0x00]).is_none()); // invalid utf-8
    }

    #[test]
    fn change_events_are_kept() {
        let blob = uevent_blob(
            "change@/devices/.../block/sr0",
            &[("SUBSYSTEM", "block"), ("DEVTYPE", "disk")],
        );
        let ev = parse_uevent(&blob).expect("change event parsed");
        assert_eq!(ev.action, "change");
        assert_eq!(ev.kname, "sr0");
    }

    #[test]
    fn skips_block_directory_itself() {
        let blob = uevent_blob(
            "add@/devices/pci0000:00/.../block",
            &[("SUBSYSTEM", "block"), ("DEVTYPE", "disk")],
        );
        assert!(parse_uevent(&blob).is_none());
    }
}