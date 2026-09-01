package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// fakeShared implements SharedClient with a scripted RenderAll response.
type fakeShared struct {
	smb, nfs string
	calls    int
}

func (f *fakeShared) RenderConfig(context.Context, *onyxv1.RenderConfigRequest, ...grpc.CallOption) (*onyxv1.RenderConfigResponse, error) {
	return nil, fmt.Errorf("unexpected RenderConfig call")
}

func (f *fakeShared) RenderAll(_ context.Context, _ *onyxv1.RenderAllRequest, _ ...grpc.CallOption) (*onyxv1.RenderAllResponse, error) {
	f.calls++
	return &onyxv1.RenderAllResponse{SmbConf: f.smb, NfsExports: f.nfs}, nil
}

// fakePrivd implements PrivdClient, recording every Run in order. failReload
// forces RELOAD_DAEMONS to exit non-zero (mirroring privd: config writes are
// atomic and succeed; reloads fail when validation rejects the file).
type fakePrivd struct {
	writes     []string // "target=content"
	reloads    [][]string
	failReload bool
}

func (f *fakePrivd) Run(_ context.Context, in *onyxv1.PrivRequest, _ ...grpc.CallOption) (*onyxv1.PrivResponse, error) {
	exit := int32(0)
	switch in.Op {
	case onyxv1.PrivOp_WRITE_DAEMON_CONFIG:
		f.writes = append(f.writes, in.Args[0]+"="+in.Args[1])
	case onyxv1.PrivOp_RELOAD_DAEMONS:
		f.reloads = append(f.reloads, append([]string{}, in.Args...))
		if f.failReload {
			exit = 1
		}
	}
	return &onyxv1.PrivResponse{ExitCode: exit}, nil
}

// applierFixture builds a configApplier wired to fake upstreams and a DB
// containing the given shares.
func applierFixture(t *testing.T, shares ...*onyxv1.Share) (*configApplier, *fakeShared, *fakePrivd) {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(dir)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range shares {
		var keys []string
		for _, p := range s.Protocols {
			k, ok := protoName(p)
			if !ok {
				t.Fatalf("bad protocol %v in fixture", p)
			}
			keys = append(keys, k)
		}
		if _, err := db.Exec(
			`INSERT INTO shares (name, path, comment, readonly, protocols, source) VALUES (?, ?, ?, ?, ?, 'manual')`,
			s.Name, s.Path, s.Comment, b2i(s.Readonly), strings.Join(keys, ","),
		); err != nil {
			t.Fatalf("insert share: %v", err)
		}
	}
	fakeS := &fakeShared{smb: "[global]", nfs: "/mnt/onyx/x  *(fsid=1,rw,no_subtree_check)"}
	fakeP := &fakePrivd{}
	return newConfigApplier(db, fakeS, fakeP), fakeS, fakeP
}

func TestConfigApplyWritesAndReloadsChangedTargets(t *testing.T) {
	a, fakeS, p := applierFixture(t,
		&onyxv1.Share{Name: "media", Path: "/mnt/onyx/media", Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB}},
	)

	if err := a.apply(context.Background()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(p.writes) != 2 {
		t.Fatalf("expected 2 target writes (smb+nfs), got %v", p.writes)
	}
	if !strings.HasPrefix(p.writes[0], "smb=[global]") {
		t.Errorf("expected smb write first, got %q", p.writes[0])
	}
	if len(p.reloads) != 1 || strings.Join(p.reloads[0], ",") != "smb,nfs" {
		t.Errorf("expected one reload of smb,nfs, got %v", p.reloads)
	}

	// Change-guard: a second apply with unchanged renders must not rewrite or
	// reload anything.
	if err := a.apply(context.Background()); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(p.writes) != 2 || len(p.reloads) != 1 {
		t.Errorf("change guard failed: writes=%v reloads=%v (want 2/1)", p.writes, p.reloads)
	}

	// A changed render (one target) rewrites only that target and reloads it.
	fakeS.smb = "[global]\nchanged = yes"
	if err := a.apply(context.Background()); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if len(p.writes) != 3 || !strings.HasPrefix(p.writes[2], "smb=") {
		t.Errorf("expected exactly one rewritten target, got %v", p.writes)
	}
	if len(p.reloads) != 2 || strings.Join(p.reloads[1], ",") != "smb" {
		t.Errorf("expected reload of only smb, got %v", p.reloads)
	}
}

func TestConfigApplyReloadFailureRetries(t *testing.T) {
	a, _, p := applierFixture(t)
	p.failReload = true

	if err := a.apply(context.Background()); err == nil {
		t.Fatal("expected apply to fail when reload exits non-zero")
	}
	// The config is on disk but the reload failed: the guard state must be
	// uncommitted so the next apply rewrites + reloads (fails closed).
	p.failReload = false
	if err := a.apply(context.Background()); err != nil {
		t.Fatalf("apply after recovery: %v", err)
	}
	// First pass: writes for smb+nfs, one failed reload. Second pass: both
	// targets rewritten, one successful reload.
	if len(p.writes) != 4 {
		t.Errorf("expected 4 writes (2 fails + 2 retries), got %v", p.writes)
	}
	if len(p.reloads) != 2 {
		t.Errorf("expected 2 reloads (fail + retry), got %v", p.reloads)
	}
}

func TestConfigApplyHandlesEmptyShareSet(t *testing.T) {
	a, _, p := applierFixture(t) // no shares
	if err := a.apply(context.Background()); err != nil {
		t.Fatalf("apply with no shares: %v", err)
	}
	if len(p.reloads) != 1 {
		t.Fatalf("expected an initial reload to sync config, got %v", p.reloads)
	}
}