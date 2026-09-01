// Command onyx-shared is the share manager (docs/design/04#1): it translates
// the logical share model (docs/design/05#6) into per-daemon config fragments
// (smb.conf share sections, /etc/exports entries). It never starts or reloads
// daemons itself — onyx-core writes the rendered config and reloads
// smbd/exportfs through onyx-privd (docs/design/02#6 steps 3-4).
package main

import (
	"context"
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

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

const version = "0.1.0-dev"

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
// (docs/design/02#6 step 3): a full smb.conf (global section + every SMB
// share) and a full exports file. Deterministic and idempotent: the same share
// set always renders the same bytes, so onyx-core can diff before writing.
// fsids are unique across the whole file (collisions on the hash resolve to
// the next free slot, deterministically — shares are processed in name order).
func (s *server) RenderAll(_ context.Context, req *onyxv1.RenderAllRequest) (*onyxv1.RenderAllResponse, error) {
	shares := req.GetShares()

	// Deterministic order for both files: sort copies by name.
	sorted := make([]*onyxv1.Share, len(shares))
	copy(sorted, shares)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var (
		b     strings.Builder
		exp   strings.Builder
		taken = map[int]bool{}
	)

	for _, share := range sorted {
		if hasProto(share, onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB) {
			b.WriteString(renderSmbConf(share))
			b.WriteString("\n")
		}
	}

	for _, share := range sorted {
		if !hasProto(share, onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS) {
			continue
		}
		fsid := 100 + fnvShare(share.Name)
		// Resolve hash collisions deterministically (sorted order → stable).
		for taken[fsid] {
			fsid++
		}
		taken[fsid] = true
		exp.WriteString(renderNfsExportsFSID(share, fsid))
	}

	resp := &onyxv1.RenderAllResponse{
		// Always emit the global skeleton — even with zero SMB shares, reload
		// must validate a real file rather than an empty one.
		SmbConf: globalSkeleton() + b.String(),
	}
	if exp.Len() > 0 {
		resp.NfsExports = exp.String()
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
		}
	}
	return resp, nil
}

// renderSmbConf emits the [share] section for smb.conf
// (docs/design/05#6, SMB row: SMB2/3, btrfs VFS, no guest by default).
func renderSmbConf(share *onyxv1.Share) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", share.Name)
	fmt.Fprintf(&b, "\tcomment = %s\n", orDefault(share.Comment, share.Name))
	fmt.Fprintf(&b, "\tpath = %s\n", share.Path)
	fmt.Fprintf(&b, "\tbrowseable = yes\n")
	fmt.Fprintf(&b, "\tread only = %s\n", yesNo(share.Readonly))
	fmt.Fprintf(&b, "\tguest ok = no\n")
	fmt.Fprintf(&b, "\tvfs objects = btrfs\n") // reflink copy-offload
	fmt.Fprintf(&b, "\tvalid users = @onyx-users\n")
	return b.String()
}

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