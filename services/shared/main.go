// Command onyx-shared is the share manager (docs/design/04#1): it translates
// the logical share model (docs/design/05#6) into per-daemon config fragments
// (smb.conf share sections, /etc/exports entries). It never starts or reloads
// daemons itself — onyx-core writes the rendered config and reloads
// smbd/exportfs through onyx-privd (docs/design/02#6 steps 3-4).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
	releaseversion "github.com/innotelinc/onyx/services/version"
)

var version = releaseversion.Version

func main() {
	var (
		socketDir = flag.String("socket-dir", "/run/onyx", "directory for onyx unix sockets")
	)
	flag.Parse()

	if err := os.MkdirAll(*socketDir, 0o750); err != nil {
		fatal("create socket dir", err)
	}

	gs := grpc.NewServer()
	srv := &server{}
	onyxv1.RegisterHealthServer(gs, srv)
	onyxv1.RegisterSharedServer(gs, srv)

	sock := absSocketPath(*socketDir, "onyx-shared.sock")
	_ = os.Remove(sock) // stale socket from a previous run
	lis, err := net.Listen("unix", sock)
	if err != nil {
		fatal("listen", err)
	}

	slog.Info("onyx-shared listening", "socket", sock, "pid", os.Getpid())

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")
		gs.GracefulStop()
	}()

	if err := gs.Serve(lis); err != nil {
		fatal("serve", err)
	}
}

func absSocketPath(dir, name string) string {
	p := filepath.Join(dir, name)
	if filepath.IsAbs(p) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func fatal(what string, err error) {
	slog.Error(what, "error", err)
	fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
	os.Exit(1)
}

// server implements Health and Shared (proto/onyx/v1).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedSharedServer
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.SharedServer = (*server)(nil)

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

// RenderAll renders the complete daemon config files for the current share set
// (docs/design/02#6 step 3): a full smb.conf, exports file, vsftpd.conf (+ one
// user file per FTP share), the dedicated sftp sshd_config, davd.conf and
// rsyncd.conf. Deterministic and idempotent: the same share set always renders
// the same bytes, so onyx-core can diff before writing. fsids are unique across
// the whole exports file (collisions on the hash resolve to the next free slot,
// deterministically — shares are processed in name order).
func (s *server) RenderAll(_ context.Context, req *onyxv1.RenderAllRequest) (*onyxv1.RenderAllResponse, error) {
	shares := req.GetShares()

	// Deterministic order for every file: sort copies by name.
	sorted := make([]*onyxv1.Share, len(shares))
	copy(sorted, shares)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var (
		smb        strings.Builder
		exp        strings.Builder
		sftp       strings.Builder
		davd       strings.Builder
		rsyncD     strings.Builder
		ftpEnabled bool
		taken      = map[int]bool{}
	)

	for _, share := range sorted {
		if hasProto(share, onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB) {
			smb.WriteString(renderSmbConf(share))
			smb.WriteString("\n")
		}
		if hasProto(share, onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS) {
			fsid := 100 + fnvShare(share.Name)
			// Resolve hash collisions deterministically (sorted order → stable).
			for taken[fsid] {
				fsid++
			}
			taken[fsid] = true
			exp.WriteString(renderNfsExportsFSID(share, fsid))
		}
		if hasProto(share, onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP) {
			// FTP is server policy plus per-user chroots; the per-user files are
			// driven by Onyx users, so the share set only decides whether the
			// vsftpd config exists at all.
			ftpEnabled = true
		}
		if hasProto(share, onyxv1.ShareProtocol_SHARE_PROTOCOL_SFTP) {
			sftp.WriteString(renderSftpMatch(share))
			sftp.WriteString("\n")
		}
		if hasProto(share, onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV) {
			davd.WriteString(renderWebdavShare(share))
			davd.WriteString("\n")
		}
		if hasProto(share, onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC) {
			rsyncD.WriteString(renderRsyncModule(share))
			rsyncD.WriteString("\n")
		}
	}

	resp := &onyxv1.RenderAllResponse{
		// Always emit the global skeleton — even with zero SMB shares, reload
		// must validate a real file rather than an empty one.
		SmbConf: globalSkeleton() + smb.String(),
	}
	if exp.Len() > 0 {
		resp.NfsExports = exp.String()
	}
	if ftpEnabled {
		resp.FtpConf = ftpGlobalSkeleton()
	}
	if sftp.Len() > 0 {
		resp.SftpConf = sftpGlobalSkeleton() + "\n" + sftp.String()
	}
	if davd.Len() > 0 {
		resp.WebdavConf = webdavGlobalSkeleton() + "\n" + davd.String()
	}
	if rsyncD.Len() > 0 {
		resp.RsyncConf = rsyncGlobalSkeleton() + "\n" + rsyncD.String()
	}
	return resp, nil
}

// globalSkeleton is the [global] section Samba needs regardless of the share
// set (docs/design/05#6 SMB row: SMB2/3 default, SMB1 disabled, user auth).
func globalSkeleton() string {
	return "# Onyx-generated Samba configuration. Managed by onyx-shared / onyx-core;\n" +
		"# do not edit by hand. (docs/design/02-technical-architecture#6)\n" +
		"[global]\n" +
		"\tworkgroup = WORKGROUP\n" +
		"\tserver string = Onyx NAS\n" +
		"\tsecurity = user\n" +
		"\tmap to guest = never\n" +
		"\tpassdb backend = tdbsam\n" +
		"\tserver min protocol = SMB2\n" +
		"\tserver max protocol = SMB3\n" +
		"\tlog file = /var/log/samba/log.%m\n" +
		"\tlogging = file\n" +
		"\tsmb ports = 445\n" +
		"\n" +
		"# The Onyx share sections below are generated per share (docs/design/05#6).\n" +
		"\n"
}

func hasProto(share *onyxv1.Share, p onyxv1.ShareProtocol) bool {
	for _, q := range share.Protocols {
		if q == p {
			return true
		}
	}
	return false
}

// RenderConfig produces daemon fragments for the share's enabled protocols
// (docs/design/05#6). Configuration is deterministic and idempotent — the same
// share always renders the same fragments, so reconciliation can diff them.
func (s *server) RenderConfig(_ context.Context, req *onyxv1.RenderConfigRequest) (*onyxv1.RenderConfigResponse, error) {
	share := req.Share
	if share == nil {
		return nil, status.Error(codes.InvalidArgument, "render request missing share")
	}
	if share.Name == "" || share.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "share requires name and path")
	}

	resp := &onyxv1.RenderConfigResponse{}
	for _, p := range share.Protocols {
		switch p {
		case onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB:
			resp.SmbConf = renderSmbConf(share)
		case onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS:
			resp.NfsExports = renderNfsExports(share)
		case onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP:
			resp.FtpConf = renderFtpShare(share)
		case onyxv1.ShareProtocol_SHARE_PROTOCOL_SFTP:
			resp.SftpConf = renderSftpMatch(share)
		case onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV:
			resp.WebdavConf = renderWebdavShare(share)
		case onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC:
			resp.RsyncConf = renderRsyncModule(share)
		}
	}
	return resp, nil
}

// renderSmbConf emits the [share] section for smb.conf
// (docs/design/05#6, SMB row: SMB2/3, btrfs VFS, no guest by default).
//
// Admission follows the share's grants (docs/design/08#2): with no grants the
// share stays open to the Onyx users group, and the first grant narrows it to
// exactly the users it names — `valid users` is what Samba enforces, so the
// Access panel is a restriction rather than a note. A `read` grant on a
// read-write share goes to `read list`, which Samba honours per connection.
func renderSmbConf(share *onyxv1.Share) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", share.Name)
	fmt.Fprintf(&b, "\tcomment = %s\n", orDefault(share.Comment, share.Name))
	fmt.Fprintf(&b, "\tpath = %s\n", share.Path)
	fmt.Fprintf(&b, "\tbrowseable = yes\n")
	fmt.Fprintf(&b, "\tread only = %s\n", yesNo(share.Readonly))
	fmt.Fprintf(&b, "\tguest ok = no\n")
	fmt.Fprintf(&b, "\tvfs objects = btrfs\n") // reflink copy-offload
	if users := accessUsers(share); len(users) > 0 {
		fmt.Fprintf(&b, "\tvalid users = %s\n", strings.Join(users, " "))
	} else {
		fmt.Fprintf(&b, "\tvalid users = @onyx-users\n")
	}
	if ro := readOnlyUsers(share); len(ro) > 0 && !share.Readonly {
		fmt.Fprintf(&b, "\tread list = %s\n", strings.Join(ro, " "))
	}
	return b.String()
}

// accessUsers lists the users a share's grants admit, sorted and de-duplicated
// so identical intent renders identical bytes. Empty means "no grants", which
// the renderers read as the group-wide default.
func accessUsers(share *onyxv1.Share) []string {
	seen := map[string]bool{}
	var users []string
	for _, a := range share.GetAccess() {
		if a.GetUsername() == "" || seen[a.GetUsername()] {
			continue
		}
		seen[a.GetUsername()] = true
		users = append(users, a.GetUsername())
	}
	sort.Strings(users)
	return users
}

// readOnlyUsers lists the users whose grant is read-only. On a share that is
// already read-only this is empty: nobody could write anyway.
func readOnlyUsers(share *onyxv1.Share) []string {
	var users []string
	for _, a := range share.GetAccess() {
		if a.GetUsername() != "" && a.GetMode() == "read" {
			users = append(users, a.GetUsername())
		}
	}
	sort.Strings(users)
	return users
}

// webdavAllowedUsers / webdavReadonlyUsers expose the same grants to davd,
// which authenticates a named user through the gateway's identity header and
// can therefore enforce them exactly (docs/design/05#6 WebDAV row).
func webdavAllowedUsers(share *onyxv1.Share) []string { return accessUsers(share) }

func webdavReadonlyUsers(share *onyxv1.Share) []string { return readOnlyUsers(share) }

// renderNfsExports emits the /etc/exports line
// (docs/design/05#6, NFS row: fsid per share, squash, not exposed by default).
func renderNfsExports(share *onyxv1.Share) string {
	// fsid = 100 + stable hash of the share name, so exports are stable across
	// renames of the export file.
	fsid := 100 + fnvShare(share.Name)
	return renderNfsExportsFSID(share, fsid)
}

// renderNfsExportsFSID is renderNfsExports with an explicit fsid (used by
// RenderAll to guarantee uniqueness across the whole exports file).
func renderNfsExportsFSID(share *onyxv1.Share, fsid int) string {
	opts := "rw"
	if share.Readonly {
		opts = "ro"
	}
	// NFSv4 with no client restriction is a deliberate skeleton default; the
	// UI will scope this per-client (docs/design/05#6).
	return fmt.Sprintf("%s  *(fsid=%d,%s,no_subtree_check,insecure)\n", share.Path, fsid, opts)
}

// --- FTP (vsftpd, docs/design/05#6 FTP row) ---

// ftpGlobalSkeleton is the shared vsftpd.conf: explicit FTPS required and
// chroot-local-user policy. Per-user chroots live in `user_config_dir` and are
// generated from Onyx users (docs/design/05#6: virtual users map to Onyx
// users), not from the share set.
func ftpGlobalSkeleton() string {
	return "# Onyx-generated vsftpd configuration. Managed by onyx-shared / onyx-core;\n" +
		"# do not edit by hand. (docs/design/05#6)\n" +
		"listen=YES\n" +
		"listen_ipv6=NO\n" +
		"anonymous_enable=NO\n" +
		"local_enable=YES\n" +
		"write_enable=YES\n" +
		"chroot_local_user=YES\n" +
		"allow_writeable_chroot=NO\n" +
		"user_config_dir=/etc/onyx/conf.d/vsftpd.d\n" +
		"# Explicit FTPS only (docs/design/05#6: TLS required by default).\n" +
		"ssl_enable=YES\n" +
		"force_local_logins_ssl=YES\n" +
		"force_local_data_ssl=YES\n" +
		"rsa_cert_file=/etc/onyx/conf.d/ftps/cert.pem\n" +
		"rsa_private_key_file=/etc/onyx/conf.d/ftps/key.pem\n" +
		"pasv_enable=YES\n" +
		"pasv_min_port=30000\n" +
		"pasv_max_port=30100\n"
}

// renderFtpShare describes how one share is exposed over FTP: its chroot root
// and write policy. Kept deterministic so change-guarding works.
func renderFtpShare(share *onyxv1.Share) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Onyx-generated vsftpd user config for share %q. Do not edit.\n", share.Name)
	fmt.Fprintf(&b, "local_root=%s\n", share.Path)
	fmt.Fprintf(&b, "write_enable=%s\n", yesNo(!share.Readonly))
	return b.String()
}

// --- SFTP (dedicated sshd, docs/design/05#6 SFTP row) ---

// sftpGlobalSkeleton is the shared sshd_config for the onyx-sftp instance: a
// separate port, SFTP-only subsystem, key auth, and a restricted group.
func sftpGlobalSkeleton() string {
	return "# Onyx-generated SFTP configuration for the dedicated onyx-sftp sshd.\n" +
		"# Managed by onyx-shared / onyx-core; do not edit by hand. (docs/design/05#6)\n" +
		"Port 2222\n" +
		"Protocol 2\n" +
		"HostKey /etc/onyx/conf.d/sftp/ssh_host_ed25519_key\n" +
		"PidFile /run/onyx/onyx-sftp.pid\n" +
		"UsePAM no\n" +
		"PasswordAuthentication no\n" +
		"PermitRootLogin no\n" +
		// Every share is a chroot, so the per-user key file has to live outside
		// it: %h would resolve inside the chroot, where a share owner could
		// replace the keys. Onyx users' keys are managed in this one directory.
		"AuthorizedKeysFile /etc/onyx/conf.d/sftp/authorized_keys/%u\n" +
		"Subsystem sftp internal-sftp\n" +
		"AllowGroups onyx-sftp\n" +
		"\n" +
		"# Per-share chroot blocks. Match blocks must stay last in sshd_config.\n"
}

// renderSftpMatch emits one `Match User onyx-<share>` block chrooting the
// share's SFTP user to its path with ForceCommand internal-sftp.
func renderSftpMatch(share *onyxv1.Share) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Match User %s\n", sftpUser(share.Name))
	fmt.Fprintf(&b, "\tChrootDirectory %s\n", share.Path)
	fmt.Fprintf(&b, "\tForceCommand internal-sftp\n")
	fmt.Fprintf(&b, "\tAllowTcpForwarding no\n")
	fmt.Fprintf(&b, "\tX11Forwarding no\n")
	fmt.Fprintf(&b, "\tPermitTTY no\n")
	return b.String()
}

// sftpUser is the SFTP login for one share (share names are [a-z0-9_-]{1,64},
// so the prefix keeps it a valid user name without escaping).
func sftpUser(share string) string {
	return "onyx-" + share
}

// --- WebDAV (onyx-davd, docs/design/05#6 WebDAV row) ---

// webdavGlobalSkeleton is the shared davd.conf. HTTPS terminates at the edge
// and davd trusts the gateway's identity header, so it listens on loopback.
func webdavGlobalSkeleton() string {
	return "# Onyx-generated WebDAV configuration for onyx-davd.\n" +
		"# Managed by onyx-shared / onyx-core; do not edit by hand. (docs/design/05#6)\n" +
		"[server]\n" +
		"listen = \"127.0.0.1:8081\"\n" +
		"tls = \"upstream\"\n" +
		"auth = \"gateway\"\n" +
		"\n" +
		"# One [[share]] table per WebDAV-enabled share.\n"
}

// renderWebdavShare emits one `[[share]]` table mapping the share to its path,
// plus the grants davd enforces for it. The lists are JSON arrays because the
// value is a list; davd rejects an unknown key rather than ignoring it, so the
// two sides cannot drift into a config that says something nobody applies.
func renderWebdavShare(share *onyxv1.Share) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[[share]]\n")
	fmt.Fprintf(&b, "name = %q\n", share.Name)
	fmt.Fprintf(&b, "path = %q\n", share.Path)
	fmt.Fprintf(&b, "readonly = %t\n", share.Readonly)
	fmt.Fprintf(&b, "allowed_users = %s\n", jsonList(webdavAllowedUsers(share)))
	fmt.Fprintf(&b, "readonly_users = %s\n", jsonList(webdavReadonlyUsers(share)))
	return b.String()
}

// jsonList renders a user list as a JSON array. json.Marshal is used (rather
// than string concatenation) so a name containing a quote is escaped instead of
// producing a config davd has to reject.
func jsonList(users []string) string {
	if users == nil {
		users = []string{}
	}
	b, err := json.Marshal(users)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// --- Rsync (rsyncd, docs/design/05#6 Rsync row) ---

// rsyncGlobalSkeleton is the shared rsyncd.conf: chrooted, authenticated
// modules, and a low connection cap.
func rsyncGlobalSkeleton() string {
	return "# Onyx-generated rsyncd configuration. Managed by onyx-shared / onyx-core;\n" +
		"# do not edit by hand. (docs/design/05#6)\n" +
		"uid = onyx\n" +
		"gid = onyx\n" +
		"use chroot = yes\n" +
		"max connections = 16\n" +
		"log file = /var/log/rsyncd.log\n" +
		"\n" +
		"# One module per rsync-enabled share.\n"
}

// renderRsyncModule emits one `[share]` module, authenticated to Onyx users.
func renderRsyncModule(share *onyxv1.Share) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", share.Name)
	fmt.Fprintf(&b, "\tpath = %s\n", share.Path)
	fmt.Fprintf(&b, "\tcomment = %s\n", orDefault(share.Comment, share.Name))
	fmt.Fprintf(&b, "\tread only = %s\n", yesNo(share.Readonly))
	fmt.Fprintf(&b, "\tauth users = onyx\n")
	return b.String()
}

// fnvShare: tiny FNV-1a hash for a stable export fsid (deterministic across
// processes and restarts).
func fnvShare(s string) int {
	const offset, prime = 2166136261, 16777619
	h := uint32(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return int(h % 100000)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
