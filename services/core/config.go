package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// configApplier closes the loop the design docs defer to "a later milestone"
// (docs/design/02#6 steps 3-4): onyx-core owns the share model (SQLite), and
// the daemon config must reflect it. Every share mutation — CreateShare,
// DeleteShare, and the hotplug reconciler — flows through apply(), which
// renders the complete daemon config for every enabled protocol via
// onyx-shared, hands changed files to onyx-privd's WRITE_DAEMON_CONFIG
// (atomic, root-owned, paths fixed by target), and reloads the affected
// daemons via RELOAD_DAEMONS.
//
// apply is change-guarded: when the rendered content for a target matches
// what we last wrote, that file is neither rewritten nor reloaded, so steady
// state is a no-op (and a rebooted privd sees untouched files). Failures
// leave the guard state uncommitted, so the next apply retries.
type configApplier struct {
	// mu serializes apply so concurrent mutations (API + reconciler) never
	// interleave render/write/reload steps.
	mu     sync.Mutex
	db     *sql.DB
	shared onyxv1.SharedClient
	privd  onyxv1.PrivdClient

	// written holds the content we last wrote for each target ("smb", "nfs",
	// "ftp", "sftp", "webdav", "rsync").
	written map[string]string
}

func newConfigApplier(db *sql.DB, shared onyxv1.SharedClient, privd onyxv1.PrivdClient) *configApplier {
	return &configApplier{
		db:      db,
		shared:  shared,
		privd:   privd,
		written: map[string]string{},
	}
}

// shares loads the share set the daemons are rendered from, with each share's
// per-user grants attached: a grant that never reached the renderer would be a
// permission the console shows and the backends ignore.
func (c *configApplier) shares(ctx context.Context) ([]*onyxv1.Share, error) {
	shares, err := listSharesForConfig(ctx, c.db)
	if err != nil {
		return nil, err
	}
	srv := &server{db: c.db}
	if err := srv.attachShareAccess(shares); err != nil {
		return nil, err
	}
	return shares, nil
}

// configTargets are reconciled unconditionally: smb.conf always carries a
// global section, and the exports file is rewritten (possibly empty) so a
// removed last NFS share stops being exported.
var configTargets = []string{"smb", "nfs"}

// optionalTargets materialize only for protocols a share actually enables. An
// empty render with nothing previously written means the protocol is off, so
// its daemon is neither written to nor reloaded (docs/design/05#6: every
// protocol is off by default).
var optionalTargets = []string{"ftp", "sftp", "webdav", "rsync"}

// apply renders the full daemon config for the current share set, writes any
// target whose content changed, and reloads the daemons that were touched.
// It is safe to call from anywhere and is a no-op in steady state.
func (c *configApplier) apply(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	shares, err := c.shares(ctx)
	if err != nil {
		return fmt.Errorf("config apply: list shares: %w", err)
	}
	rendered, err := c.shared.RenderAll(ctx, &onyxv1.RenderAllRequest{Shares: shares})
	if err != nil {
		return fmt.Errorf("config apply: render all: %w", err)
	}

	files := map[string]string{
		"smb":    rendered.SmbConf,
		"nfs":    rendered.NfsExports,
		"ftp":    rendered.FtpConf,
		"sftp":   rendered.SftpConf,
		"webdav": rendered.WebdavConf,
		"rsync":  rendered.RsyncConf,
	}

	var changed []string
	for _, target := range configTargets {
		if err := c.writeTarget(ctx, &changed, target, files[target]); err != nil {
			return err
		}
	}
	for _, target := range optionalTargets {
		content := files[target]
		if content == "" && c.written[target] == "" {
			continue
		}
		if err := c.writeTarget(ctx, &changed, target, content); err != nil {
			return err
		}
	}

	if len(changed) == 0 {
		return nil
	}
	resp, err := c.privd.Run(ctx, &onyxv1.PrivRequest{
		Op:   onyxv1.PrivOp_RELOAD_DAEMONS,
		Args: changed,
	})
	if err := privdOK(resp, err, "reload daemons"); err != nil {
		// Reload failed (e.g. testparm rejected the file). Forget what we
		// recorded so the next apply rewrites + retries; the config on disk
		// is the invalid one, and failing closed beats serving it.
		for _, t := range changed {
			delete(c.written, t)
		}
		return err
	}
	slog.Info("daemon config written and reloaded", "targets", strings.Join(changed, ","))
	return nil
}

// writeTarget writes one daemon config target when its content changed and, on
// success, records what is now on disk and marks the owning daemon for reload.
func (c *configApplier) writeTarget(ctx context.Context, changed *[]string, target, content string) error {
	if c.written[target] == content {
		return nil
	}
	resp, err := c.privd.Run(ctx, &onyxv1.PrivRequest{
		Op:   onyxv1.PrivOp_WRITE_DAEMON_CONFIG,
		Args: []string{target, content},
	})
	if err := privdOK(resp, err, "write "+target+" config"); err != nil {
		return err
	}
	c.written[target] = content
	if !containsStr(*changed, target) {
		*changed = append(*changed, target)
	}
	return nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// listSharesForConfig loads the full share set in the deterministic order
// onyx-shared expects (it re-sorts by name, but keep DB reads ordered).
func listSharesForConfig(ctx context.Context, db *sql.DB) ([]*onyxv1.Share, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, path, comment, readonly, protocols FROM shares ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shares []*onyxv1.Share
	for rows.Next() {
		share, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		shares = append(shares, share)
	}
	return shares, rows.Err()
}

// privdOK converts a privd call into an error, checking both transport
// errors and the exit code privd returns in-band for command failure.
func privdOK(resp *onyxv1.PrivResponse, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.ExitCode != 0 {
		return status.Errorf(codes.Internal, "%s: privd exit %d: %s", what, resp.ExitCode, bytesTrim(resp.Stderr))
	}
	return nil
}

func bytesTrim(b []byte) string {
	s := string(b)
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
