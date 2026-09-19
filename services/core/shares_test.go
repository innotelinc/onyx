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

// A grant is the record the share backends enforce, so it has to round trip,
// follow the share it names, refuse junk, and disappear with its share —
// otherwise the console and the daemons drift apart (docs/design/08#2).
func TestShareAccessRoundTripAndValidation(t *testing.T) {
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &server{db: db}
	ctx := context.Background()

	if _, err := s.CreateShare(ctx, &onyxv1.CreateShareRequest{
		Name: "media", Path: "/mnt/onyx/media",
		Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB, onyxv1.ShareProtocol_SHARE_PROTOCOL_WEBDAV},
	}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	set := func(share, user, mode string) error {
		_, err := s.SetShareAccess(ctx, &onyxv1.SetShareAccessRequest{
			Access: &onyxv1.ShareAccess{Share: share, Username: user, Mode: mode},
		})
		return err
	}
	if err := set("media", "alice", "read-write"); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	if err := set("media", "bob", "read"); err != nil {
		t.Fatalf("grant bob: %v", err)
	}

	// The share reports its own grants, which is what the Access panel reads.
	share, err := s.GetShare(ctx, &onyxv1.GetShareRequest{Name: "media"})
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	modes := map[string]string{}
	for _, a := range share.Access {
		modes[a.Username] = a.Mode
	}
	if modes["alice"] != "read-write" || modes["bob"] != "read" {
		t.Errorf("share access = %v, want alice=read-write bob=read", modes)
	}

	// Re-granting changes the mode instead of adding a second row.
	if err := set("media", "alice", "read"); err != nil {
		t.Fatalf("regrant alice: %v", err)
	}
	all, err := s.ListShareAccess(ctx, &onyxv1.ListShareAccessRequest{})
	if err != nil {
		t.Fatalf("ListShareAccess: %v", err)
	}
	if len(all.Access) != 2 {
		t.Fatalf("expected 2 grants after regrant, got %+v", all.Access)
	}

	// Filtering by user is how the Users page answers "what can they reach".
	one, err := s.ListShareAccess(ctx, &onyxv1.ListShareAccessRequest{Username: "bob"})
	if err != nil {
		t.Fatalf("ListShareAccess(bob): %v", err)
	}
	if len(one.Access) != 1 || one.Access[0].Share != "media" {
		t.Errorf("bob's grants = %+v", one.Access)
	}

	// Junk is refused: a mode nobody renders, an unknown share, and a username
	// that could not be read back out of a config file.
	if err := set("media", "alice", "owner"); err == nil {
		t.Error("unknown mode must be refused")
	}
	if err := set("ghost", "alice", "read"); err == nil {
		t.Error("a grant on an unknown share must be refused")
	}
	if err := set("media", "bad user", "read"); err == nil {
		t.Error("a username with whitespace must be refused")
	}

	// An empty mode removes the grant rather than storing a third state.
	if err := set("media", "bob", ""); err != nil {
		t.Fatalf("remove bob: %v", err)
	}
	one, err = s.ListShareAccess(ctx, &onyxv1.ListShareAccessRequest{Share: "media"})
	if err != nil {
		t.Fatalf("ListShareAccess(media): %v", err)
	}
	if len(one.Access) != 1 || one.Access[0].Username != "alice" {
		t.Errorf("after removal = %+v, want only alice", one.Access)
	}

	// Deleting the share forgets its grants, so reusing the name cannot
	// resurrect an old permission.
	if _, err := s.DeleteShare(ctx, &onyxv1.DeleteShareRequest{Name: "media"}); err != nil {
		t.Fatalf("DeleteShare: %v", err)
	}
	one, err = s.ListShareAccess(ctx, &onyxv1.ListShareAccessRequest{Share: "media"})
	if err != nil {
		t.Fatalf("ListShareAccess after delete: %v", err)
	}
	if len(one.Access) != 0 {
		t.Errorf("grants survived the share: %+v", one.Access)
	}
}
