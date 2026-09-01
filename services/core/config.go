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

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// configApplier closes the loop the design docs defer to "a later milestone"
// (docs/design/02#6 steps 3-4): onyx-core owns the share model (SQLite), and
// the daemon config must reflect it. Every share mutation — CreateShare,
// DeleteShare, and the hotplug reconciler — flows through apply(), which
// renders the complete smb.conf + exports via onyx-shared, hands changed
// files to onyx-privd's WRITE_DAEMON_CONFIG (atomic, root-owned, paths fixed
// by target), and reloads the affected daemons (testparm-validated) via
// RELOAD_DAEMONS.
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

	// written holds the content we last wrote for each target ("smb", "nfs").
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

// configTargets is the fixed set of daemon config files privd knows how to
// write (docs/design/02#6): smb.conf and the NFS exports file.
var configTargets = []string{"smb", "nfs"}

// apply renders the full daemon config for the current share set, writes any
// target whose content changed, and reloads the daemons that were touched.
// It is safe to call from anywhere and is a no-op in steady state.
func (c *configApplier) apply(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	shares, err := listSharesForConfig(ctx, c.db)
	if err != nil {
		return fmt.Errorf("config apply: list shares: %w", err)
	}
	rendered, err := c.shared.RenderAll(ctx, &onyxv1.RenderAllRequest{Shares: shares})
	if err != nil {
		return fmt.Errorf("config apply: render all: %w", err)
	}

	files := map[string]string{
		"smb": rendered.SmbConf,
		"nfs": rendered.NfsExports,
	}

	var changed []string
	for _, target := range configTargets {
		content := files[target]
		if c.written[target] == content {
			continue
		}
		resp, err := c.privd.Run(ctx, &onyxv1.PrivRequest{
			Op:   onyxv1.PrivOp_WRITE_DAEMON_CONFIG,
			Args: []string{target, content},
		})
		if err := privdOK(resp, err, "write "+target+" config"); err != nil {
			return err
		}
		c.written[target] = content
		changed = append(changed, target)
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