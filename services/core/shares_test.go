package main

import (
	"context"
	"testing"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// Every protocol in the contract must survive the DB round trip
// (protoName -> key -> protoFromName), so a restart never silently drops one
// when the protocol surface grows (docs/design/05#6).
func TestProtoNameRoundTrip(t *testing.T) {
	for name, value := range onyxv1.ShareProtocol_value {
		proto := onyxv1.ShareProtocol(value)
		if proto == onyxv1.ShareProtocol_SHARE_PROTOCOL_UNSPECIFIED {
			continue
		}
		key, ok := protoName(proto)
		if !ok {
			t.Fatalf("protoName(%v [%s]) unsupported", proto, name)
		}
		back, ok := protoFromName(key)
		if !ok || back != proto {
			t.Fatalf("round trip %v -> %q -> %v (resolved=%v)", proto, key, back, ok)
		}
	}

	// Junk never resolves, in either direction.
	if _, ok := protoFromName("httpd"); ok {
		t.Error("unknown protocol name must not resolve")
	}
	if _, ok := protoName(onyxv1.ShareProtocol(99)); ok {
		t.Error("unknown protocol enum must not resolve")
	}
}

// A share created with the full protocol surface must read back with all of
// them: the DB stores protocol keys, not enum values.
func TestShareProtocolsPersist(t *testing.T) {
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &server{db: db}

	req := &onyxv1.CreateShareRequest{
		Name: "media",
		Path: "/mnt/onyx/media",
		Protocols: []onyxv1.ShareProtocol{
			onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_NFS,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_FTP,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_SFTP,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV,
			onyxv1.ShareProtocol_SHARE_PROTOCOL_RSYNC,
		},
	}
	if _, err := s.CreateShare(context.Background(), req); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	got, err := s.GetShare(context.Background(), &onyxv1.GetShareRequest{Name: "media"})
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	seen := map[onyxv1.ShareProtocol]bool{}
	for _, p := range got.Protocols {
		seen[p] = true
	}
	for _, p := range req.Protocols {
		if !seen[p] {
			t.Errorf("protocol %v lost in the DB round trip (got %v)", p, got.Protocols)
		}
	}

	// An unsupported protocol is rejected rather than persisted.
	if _, err := s.CreateShare(context.Background(), &onyxv1.CreateShareRequest{
		Name: "bad", Path: "/mnt/onyx/bad",
		Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol(99)},
	}); err == nil {
		t.Error("unsupported protocol must be rejected")
	}
}
