package main

import (
	"context"
	"strings"
	"testing"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

func TestRenderAllFullSmbConf(t *testing.T) {
	s := &server{}
	resp, err := s.RenderAll(context.Background(), &onyxv1.RenderAllRequest{
		Shares: []*onyxv1.Share{
			{Name: "media", Path: "/mnt/onyx/media", Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB}},
			{Name: "backup", Path: "/mnt/onyx/backup", Comment: "Nightly backups", Readonly: true,
				Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB, onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS}},
		},
	})
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}

	smb := resp.SmbConf
	for _, want := range []string{
		"[global]",
		"workgroup = WORKGROUP",
		"server min protocol = SMB2",
		"[media]",
		"path = /mnt/onyx/media",
		"[backup]",
		"comment = Nightly backups",
		"read only = yes",
	} {
		if !strings.Contains(smb, want) {
			t.Errorf("smb.conf missing %q:\n%s", want, smb)
		}
	}
	// Section order is deterministic: [global] first, then shares in name order.
	iGlobal := strings.Index(smb, "[global]")
	iMedia := strings.Index(smb, "[media]")
	iBackup := strings.Index(smb, "[backup]")
	if !(iGlobal >= 0 && iGlobal < iBackup && iBackup < iMedia) {
		t.Errorf("smb.conf sections out of order:\n%s", smb)
	}

	// NFS: only the NFS-enabled share is exported, with a unique fsid.
	exp := resp.NfsExports
	if !strings.Contains(exp, "/mnt/onyx/backup") {
		t.Errorf("exports missing backup:\n%s", exp)
	}
	if strings.Contains(exp, "/mnt/onyx/media") {
		t.Errorf("exports must not contain non-NFS share media:\n%s", exp)
	}
}

func TestRenderAllUniqueFsidsAndEmpty(t *testing.T) {
	s := &server{}

	// Two shares whose hash collisions resolve to distinct fsids.
	resp, err := s.RenderAll(context.Background(), &onyxv1.RenderAllRequest{
		Shares: []*onyxv1.Share{
			{Name: "aaa", Path: "/mnt/onyx/aaa", Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS}},
			{Name: "zzz", Path: "/mnt/onyx/zzz", Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS}},
		},
	})
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	fsid := map[string]bool{}
	for _, line := range strings.Split(resp.NfsExports, "\n") {
		if line == "" {
			continue
		}
		for _, part := range strings.Split(line[strings.Index(line, "(")+1:strings.Index(line, ")")], ",") {
			if strings.HasPrefix(part, "fsid=") {
				if fsid[part[5:]] {
					t.Errorf("duplicate fsid %s in:\n%s", part[5:], resp.NfsExports)
				}
				fsid[part[5:]] = true
			}
		}
	}

	// Empty share set still yields a global-only smb.conf (reload-safe).
	resp, err = s.RenderAll(context.Background(), &onyxv1.RenderAllRequest{})
	if err != nil {
		t.Fatalf("RenderAll empty: %v", err)
	}
	if !strings.Contains(resp.SmbConf, "[global]") {
		t.Errorf("empty set must still render [global], got:\n%s", resp.SmbConf)
	}
	if resp.NfsExports != "" {
		t.Errorf("empty set must render no exports, got:\n%s", resp.NfsExports)
	}
}

func TestRenderAllDeterministic(t *testing.T) {
	s := &server{}
	shares := []*onyxv1.Share{
		{Name: "zebra", Path: "/mnt/onyx/zebra", Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB}},
		{Name: "alpha", Path: "/mnt/onyx/alpha", Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB, onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS}},
	}
	a, err := s.RenderAll(context.Background(), &onyxv1.RenderAllRequest{Shares: shares})
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	// Same set, shuffled input order → identical bytes.
	b, err := s.RenderAll(context.Background(), &onyxv1.RenderAllRequest{Shares: []*onyxv1.Share{shares[1], shares[0]}})
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	if a.SmbConf != b.SmbConf || a.NfsExports != b.NfsExports {
		t.Errorf("RenderAll not deterministic:\n--- a ---\n%q\n--- b ---\n%q", a.SmbConf, b.SmbConf)
	}
}