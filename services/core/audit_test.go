package main

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// auditServer builds a core server backed by a fresh DB.
func auditServer(t *testing.T) *server {
	t.Helper()
	db, err := openDB(t.TempDir())
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &server{db: db}
}

// Setting a grant is a "grant" event, clearing one is a "revoke" — the trail
// must say which, and who did it, or it cannot answer the question it exists for.
func TestRecordAccessEventClassifiesChanges(t *testing.T) {
	s := auditServer(t)
	ctx := context.Background()

	if got := accessEventKind("read-write"); got != eventGrant {
		t.Errorf("grant kind = %q", got)
	}
	if got := accessEventKind(""); got != eventRevoke {
		t.Errorf("revoke kind = %q", got)
	}
	// An unattributed change (a script through the CLI) is named, not blank.
	if got := accessEventActor("  "); got != "console" {
		t.Errorf("actor = %q", got)
	}
	if got := accessEventActor("dana"); got != "dana" {
		t.Errorf("actor = %q", got)
	}

	for _, ev := range []struct{ kind, mode string }{
		{eventGrant, "read"},
		{eventRevoke, ""},
	} {
		if err := s.recordAccessEvent(ctx, ev.kind, "media", "dana", ev.mode, "  ", ""); err != nil {
			t.Fatalf("recordAccessEvent: %v", err)
		}
	}

	resp, err := s.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{})
	if err != nil {
		t.Fatalf("ListAccessEvents: %v", err)
	}
	if len(resp.GetEvents()) != 2 {
		t.Fatalf("events = %d, want 2", len(resp.GetEvents()))
	}
	// Newest first: the revoke was recorded last.
	first := resp.GetEvents()[0]
	if first.GetKind() != eventRevoke || first.GetShare() != "media" || first.GetActor() != "console" {
		t.Errorf("newest event = %+v", first)
	}
	if second := resp.GetEvents()[1]; second.GetKind() != eventGrant || second.GetMode() != "read" {
		t.Errorf("oldest event = %+v", second)
	}
}

// Deleting a share takes its grants with it — a row naming a share that no
// longer exists would come back as an access rule if the name were reused — but
// the trail stays: who was granted and who was refused is the evidence an audit
// trail exists to keep.
func TestDeleteShareKeepsTheTrailButDropsTheGrants(t *testing.T) {
	s := auditServer(t)
	ctx := context.Background()
	if _, err := s.CreateShare(ctx, &onyxv1.CreateShareRequest{
		Name:      "media",
		Path:      "/mnt/onyx/media",
		Protocols: []onyxv1.ShareProtocol{onyxv1.ShareProtocol_SHARE_PROTOCOL_SMB},
	}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if _, err := s.SetShareAccess(ctx, &onyxv1.SetShareAccessRequest{
		Access: &onyxv1.ShareAccess{Share: "media", Username: "dana", Mode: "read"},
		Actor:  "ada",
	}); err != nil {
		t.Fatalf("SetShareAccess: %v", err)
	}
	if _, err := s.RecordAccessDenial(ctx, &onyxv1.RecordAccessDenialRequest{
		Share: "media", Username: "mallory", Method: "PUT", Reason: "not granted",
	}); err != nil {
		t.Fatalf("RecordAccessDenial: %v", err)
	}

	if _, err := s.DeleteShare(ctx, &onyxv1.DeleteShareRequest{Name: "media"}); err != nil {
		t.Fatalf("DeleteShare: %v", err)
	}
	grants, err := s.ListShareAccess(ctx, &onyxv1.ListShareAccessRequest{Share: "media"})
	if err != nil {
		t.Fatalf("ListShareAccess: %v", err)
	}
	if len(grants.GetAccess()) != 0 {
		t.Errorf("grants survived the share being deleted: %v", grants.GetAccess())
	}
	events, err := s.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{Share: "media"})
	if err != nil {
		t.Fatalf("ListAccessEvents: %v", err)
	}
	if len(events.GetEvents()) != 2 {
		t.Fatalf("the trail was erased with the share: %d events", len(events.GetEvents()))
	}
}

// A denial is recorded against the share it was refused for; a denial with no
// share would be unattributable, so it is refused.
func TestRecordAccessDenial(t *testing.T) {
	s := auditServer(t)
	ctx := context.Background()

	if _, err := s.RecordAccessDenial(ctx, &onyxv1.RecordAccessDenialRequest{Username: "mallory"}); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("a denial without a share must be refused, got %v", err)
	}

	if _, err := s.RecordAccessDenial(ctx, &onyxv1.RecordAccessDenialRequest{
		Share:    "media",
		Username: "mallory",
		Method:   "PUT",
		Reason:   "not granted the share",
	}); err != nil {
		t.Fatalf("RecordAccessDenial: %v", err)
	}

	resp, err := s.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{Share: "media"})
	if err != nil {
		t.Fatalf("ListAccessEvents: %v", err)
	}
	if len(resp.GetEvents()) != 1 {
		t.Fatalf("events = %d, want 1", len(resp.GetEvents()))
	}
	got := resp.GetEvents()[0]
	if got.GetKind() != eventDenied || got.GetUsername() != "mallory" {
		t.Errorf("event = %+v", got)
	}
	if got.GetDetail() != "PUT not granted the share" {
		t.Errorf("detail = %q", got.GetDetail())
	}
}

// The trail is browsed in pages, so the share filter must narrow it and the
// limit must actually bound it (and never let a caller ask for the whole table).
func TestListAccessEventsFiltersAndLimits(t *testing.T) {
	s := auditServer(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.recordAccessEvent(ctx, eventGrant, "media", "dana", "read", "console", ""); err != nil {
			t.Fatalf("recordAccessEvent: %v", err)
		}
	}
	if err := s.recordAccessEvent(ctx, eventGrant, "backups", "erin", "read", "console", ""); err != nil {
		t.Fatalf("recordAccessEvent: %v", err)
	}

	all, err := s.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{})
	if err != nil {
		t.Fatalf("ListAccessEvents: %v", err)
	}
	if len(all.GetEvents()) != 6 {
		t.Fatalf("unfiltered events = %d, want 6", len(all.GetEvents()))
	}

	filtered, err := s.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{Share: "backups"})
	if err != nil {
		t.Fatalf("ListAccessEvents: %v", err)
	}
	if len(filtered.GetEvents()) != 1 || filtered.GetEvents()[0].GetUsername() != "erin" {
		t.Fatalf("filtered events = %+v", filtered.GetEvents())
	}

	limited, err := s.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{Limit: 2})
	if err != nil {
		t.Fatalf("ListAccessEvents: %v", err)
	}
	if len(limited.GetEvents()) != 2 {
		t.Fatalf("limited events = %d, want 2", len(limited.GetEvents()))
	}

	// A negative or oversized limit falls back to the page size rather than
	// failing the request or dumping everything.
	if resp, err := s.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{Limit: -1}); err != nil || len(resp.GetEvents()) != 6 {
		t.Errorf("negative limit: %d events, err=%v", len(resp.GetEvents()), err)
	}
	if resp, err := s.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{Limit: maxAccessEventLimit + 1000}); err != nil || len(resp.GetEvents()) != 6 {
		t.Errorf("oversized limit: %d events, err=%v", len(resp.GetEvents()), err)
	}
}
