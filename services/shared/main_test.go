package main

import (
	"context"
	"strings"
	"testing"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
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

func TestRenderAllProtocolSurface(t *testing.T) {
	s := &server{}
	resp, err := s.RenderAll(context.Background(), &onyxv1.RenderAllRequest{
		Shares: []*onyxv1.Share{
			{Name: "media", Path: "/mnt/onyx/media", Comment: "Media",
				Protocols: []onyxv1.ShareProtocol{
					onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP,
					onyxv1.ShareProtocol_SHARE_PROTOCOL_SFTP,
					onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV,
					onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC}},
			{Name: "backup", Path: "/mnt/onyx/backup", Readonly: true,
				Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC}},
		},
	})
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}

	// FTP: global file + one user file per FTP share, name-sorted.
	for _, want := range []string{"user_config_dir=/etc/onyx/conf.d/vsftpd.d", "force_local_logins_ssl=YES"} {
		if !strings.Contains(resp.FtpConf, want) {
			t.Errorf("vsftpd.conf missing %q:\n%s", want, resp.FtpConf)
		}
	}
	// SFTP: dedicated instance with a Match block per share.
	for _, want := range []string{"Port 2222", "Subsystem sftp internal-sftp", "Match User onyx-media",
		"ChrootDirectory /mnt/onyx/media", "ForceCommand internal-sftp"} {
		if !strings.Contains(resp.SftpConf, want) {
			t.Errorf("sshd_config missing %q:\n%s", want, resp.SftpConf)
		}
	}

	// WebDAV: one share table per WebDAV share.
	if !strings.Contains(resp.WebdavConf, "[[share]]") || !strings.Contains(resp.WebdavConf, `path = "/mnt/onyx/media"`) {
		t.Errorf("davd.conf missing share table:\n%s", resp.WebdavConf)
	}

	// Rsync: both modules, readonly reflected.
	for _, want := range []string{"[media]", "[backup]", "path = /mnt/onyx/media", "read only = yes", "auth users = onyx"} {
		if !strings.Contains(resp.RsyncConf, want) {
			t.Errorf("rsyncd.conf missing %q:\n%s", want, resp.RsyncConf)
		}
	}

	// Protocols nobody enabled stay empty (off by default).
	if resp.NfsExports != "" {
		t.Errorf("no share enabled NFS, got:\n%s", resp.NfsExports)
	}
	if strings.Contains(resp.SmbConf, "[media]") {
		t.Errorf("no share enabled SMB, got:\n%s", resp.SmbConf)
	}
}

func TestRenderConfigProtocolFragments(t *testing.T) {
	s := &server{}
	share := &onyxv1.Share{
		Name: "docs", Path: "/mnt/onyx/docs", Comment: "Docs", Readonly: true,
		Protocols: []onyxv1.ShareProtocol{
			onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_SFTP,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC,
		},
	}
	rc, err := s.RenderConfig(context.Background(), &onyxv1.RenderConfigRequest{Share: share})
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}
	if !strings.Contains(rc.FtpConf, "local_root=/mnt/onyx/docs") || !strings.Contains(rc.FtpConf, "write_enable=no") {
		t.Errorf("ftp fragment wrong:\n%s", rc.FtpConf)
	}
	if !strings.Contains(rc.SftpConf, "Match User onyx-docs") {
		t.Errorf("sftp fragment wrong:\n%s", rc.SftpConf)
	}
	if !strings.Contains(rc.WebdavConf, "readonly = true") {
		t.Errorf("webdav fragment wrong:\n%s", rc.WebdavConf)
	}
	if !strings.Contains(rc.RsyncConf, "[docs]") || !strings.Contains(rc.RsyncConf, "read only = yes") {
		t.Errorf("rsync fragment wrong:\n%s", rc.RsyncConf)
	}
	// SMB/NFS stay empty when not enabled.
	if rc.SmbConf != "" || rc.NfsExports != "" {
		t.Errorf("unexpected smb/nfs fragments: %q / %q", rc.SmbConf, rc.NfsExports)
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

func TestRenderAllProtocolSurfaceDeterministic(t *testing.T) {
	s := &server{}
	shares := []*onyxv1.Share{
		{Name: "zebra", Path: "/mnt/onyx/zebra", Protocols: []onyxv1.ShareProtocol{
			onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP, onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC}},
		{Name: "alpha", Path: "/mnt/onyx/alpha", Readonly: true, Protocols: []onyxv1.ShareProtocol{
			onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP, onyxv1.ShareProtocol_SHARE_PROTOCOL_SFTP,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV, onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC}},
	}
	a, err := s.RenderAll(context.Background(), &onyxv1.RenderAllRequest{Shares: shares})
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	b, err := s.RenderAll(context.Background(), &onyxv1.RenderAllRequest{Shares: []*onyxv1.Share{shares[1], shares[0]}})
	if err != nil {
		t.Fatalf("RenderAll: %v", err)
	}
	if a.FtpConf != b.FtpConf || a.SftpConf != b.SftpConf || a.WebdavConf != b.WebdavConf || a.RsyncConf != b.RsyncConf {
		t.Errorf("RenderAll protocol surface not deterministic")
	}
}
