package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// fakeCore is the slice of onyx-core the access endpoints use: the Samba
// account list they show, and the audit trail they read. The embedded interface
// is nil, so an endpoint that reaches for anything else fails loudly instead of
// silently returning a zero value.
type fakeCore struct {
	onyxv1.CoreClient
	sambaAccounts []string
	sambaErr      error
	sambaCalls    []string // "add dana secret:correct horse"
	events        []*onyxv1.AccessEvent
	eventErr      error
	eventReq      *onyxv1.ListAccessEventsRequest
}

func (f *fakeCore) ProvisionSambaUser(_ context.Context, in *onyxv1.ProvisionSambaUserRequest, _ ...grpc.CallOption) (*onyxv1.ProvisionSambaUserResponse, error) {
	if in.GetAction() != "list" {
		f.sambaCalls = append(f.sambaCalls, strings.Join([]string{in.GetAction(), in.GetUsername()}, " ")+" secret:"+in.GetPassword())
	}
	if f.sambaErr != nil {
		return nil, f.sambaErr
	}
	// Mirrors core: only an add leaves a name Samba can sign in with.
	return &onyxv1.ProvisionSambaUserResponse{Provisioned: in.GetAction() == "add", Accounts: f.sambaAccounts}, nil
}

func (f *fakeCore) ListAccessEvents(_ context.Context, in *onyxv1.ListAccessEventsRequest, _ ...grpc.CallOption) (*onyxv1.ListAccessEventsResponse, error) {
	f.eventReq = in
	if f.eventErr != nil {
		return nil, f.eventErr
	}
	return &onyxv1.ListAccessEventsResponse{Events: f.events}, nil
}

// accessFixture builds the gateway over a share store and a core stub, with one
// Onyx user to act on.
func accessFixture(t *testing.T) (*server, *fakeCoreShares, *fakeCore, string) {
	t.Helper()
	store, err := newUserStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUserStore: %v", err)
	}
	if _, err := store.upsert("alice", func(*onyxUser) {}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	shares := newFakeCoreShares()
	core := &fakeCore{}
	return &server{users: store, coreShares: shares, core: core}, shares, core, store.list()[0].ID
}

func getJSON(t *testing.T, s *server, method, target, body string, handler http.HandlerFunc, pathValue ...string) (int, map[string]any, string) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if len(pathValue) == 2 {
		req.SetPathValue(pathValue[0], pathValue[1])
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	text := rec.Body.String()
	decoded := map[string]any{}
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			t.Fatalf("%s %s returned unreadable JSON: %v\n%s", method, target, err, text)
		}
	}
	return rec.Code, decoded, text
}

// modesOf flattens an `access` array into "share/user=mode", which reads better
// in a failure message than a slice of maps.
func modesOf(t *testing.T, decoded map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, _ := decoded["access"].([]any)
	for _, entry := range entries {
		record, _ := entry.(map[string]any)
		share, _ := record["share"].(string)
		user, _ := record["username"].(string)
		mode, _ := record["mode"].(string)
		out[share+"/"+user] = mode
	}
	return out
}

// The shares page asks one share at a time, and needs to know which grantees
// Samba will actually accept: a grant names a user in smb.conf's `valid users`,
// and Samba treats a name it has no account for as nobody.
func TestShareAccessListsGrantsAndSambaAccounts(t *testing.T) {
	s, shares, core, _ := accessFixture(t)
	core.sambaAccounts = []string{"alice", "dana"}
	ctx := context.Background()
	for _, g := range []struct{ share, user, mode string }{
		{"media", "dana", "read"},
		{"media", "erin", "read-write"},
		{"docs", "frank", "read"},
	} {
		if _, err := shares.SetShareAccess(ctx, &onyxv1.SetShareAccessRequest{
			Access: &onyxv1.ShareAccess{Share: g.share, Username: g.user, Mode: g.mode},
		}); err != nil {
			t.Fatalf("seed grant: %v", err)
		}
	}

	code, decoded, text := getJSON(t, s, http.MethodGet, "/api/v1/shares/media/access", "", s.handleShareAccess, "name", "media")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	got := modesOf(t, decoded)
	if len(got) != 2 || got["media/dana"] != "read" || got["media/erin"] != "read-write" {
		t.Errorf("grants = %v, want only media's two", got)
	}
	accounts, _ := decoded["samba_accounts"].([]any)
	if len(accounts) != 2 {
		t.Errorf("samba_accounts = %v, want the two core reports", decoded["samba_accounts"])
	}

	// A share nobody is granted answers with an empty list rather than null, so
	// the console does not have to special-case it.
	code, empty, text := getJSON(t, s, http.MethodGet, "/api/v1/shares/unused/access", "", s.handleShareAccess, "name", "unused")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if list, ok := empty["access"].([]any); !ok || len(list) != 0 {
		t.Errorf("empty share access = %v, want []", empty["access"])
	}
}

// A deployment without Samba has no passdb to read, but its grants are still
// real: the page must list them and say the Samba half is unavailable, not fail
// the whole view.
func TestShareAccessWithoutSambaStillListsGrants(t *testing.T) {
	s, shares, core, _ := accessFixture(t)
	core.sambaErr = status.Error(codes.Unavailable, "onyx-privd is not reachable")
	if _, err := shares.SetShareAccess(context.Background(), &onyxv1.SetShareAccessRequest{
		Access: &onyxv1.ShareAccess{Share: "media", Username: "dana", Mode: "read"},
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	code, decoded, text := getJSON(t, s, http.MethodGet, "/api/v1/shares/media/access", "", s.handleShareAccess, "name", "media")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if got := modesOf(t, decoded); got["media/dana"] != "read" {
		t.Errorf("grants = %v", got)
	}
	if accounts, ok := decoded["samba_accounts"].([]any); !ok || len(accounts) != 0 {
		t.Errorf("samba_accounts = %v, want an empty list", decoded["samba_accounts"])
	}
}

// The Shares page's access panel saves the whole map at once. Only what changed
// is written, because every write re-renders the daemon config the backends
// serve, and the change is attributed to the operator who made it.
func TestSetShareAccessWritesOnlyTheDiffWithActor(t *testing.T) {
	s, shares, _, _ := accessFixture(t)
	ctx := context.Background()
	if _, err := shares.SetShareAccess(ctx, &onyxv1.SetShareAccessRequest{
		Access: &onyxv1.ShareAccess{Share: "media", Username: "dana", Mode: "read"},
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	shares.calls = nil

	req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/media/access",
		strings.NewReader(`{"access":{"dana":"read","erin":"read-write"}}`))
	req.SetPathValue("name", "media")
	req.Header.Set("X-Onyx-User", "ada")
	rec := httptest.NewRecorder()
	s.handleSetShareAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if len(shares.calls) != 1 || shares.calls[0] != "set media erin read-write" {
		t.Errorf("calls = %v, want only erin's new grant", shares.calls)
	}
	if shares.actors["media\x00erin"] != "ada" {
		t.Errorf("actor = %q, want the operator named on the request", shares.actors["media\x00erin"])
	}

	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := modesOf(t, decoded); len(got) != 2 {
		t.Errorf("response grants = %v", got)
	}

	// Leaving somebody out of the map removes their grant: that is what the
	// panel's "No access" sends.
	req = httptest.NewRequest(http.MethodPut, "/api/v1/shares/media/access", strings.NewReader(`{"access":{"erin":"read-write"}}`))
	req.SetPathValue("name", "media")
	rec = httptest.NewRecorder()
	s.handleSetShareAccess(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if _, ok := shares.grants["media\x00dana"]; ok {
		t.Error("dana's grant survived being left out of the map")
	}
}

// A mode the renderers cannot express is refused before anything is written:
// storing "owner" would render as nothing and mean nothing.
func TestSetShareAccessRejectsBadRequests(t *testing.T) {
	s, shares, _, _ := accessFixture(t)
	cases := []struct {
		name string
		body string
	}{
		{"unknown mode", `{"access":{"dana":"owner"}}`},
		{"malformed body", `{"access":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v1/shares/media/access", strings.NewReader(tc.body))
			req.SetPathValue("name", "media")
			rec := httptest.NewRecorder()
			s.handleSetShareAccess(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
	if len(shares.calls) != 0 {
		t.Errorf("a rejected request reached core: %v", shares.calls)
	}
}

// A grant over SMB only works if the grantee has a Samba account, so the
// Users page can set one. The password is handed to core and never echoed back.
func TestSambaPasswordEndpoints(t *testing.T) {
	s, _, core, id := accessFixture(t)
	core.sambaAccounts = []string{"alice"}

	code, decoded, text := getJSON(t, s, http.MethodPost, "/api/v1/users/"+id+"/smb-password",
		`{"password":"correct horse"}`, s.handleSetSambaPassword, "id", id)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if decoded["provisioned"] != true {
		t.Errorf("provisioned = %v, want true", decoded["provisioned"])
	}
	if strings.Contains(text, "correct horse") {
		t.Errorf("the password was echoed back: %s", text)
	}
	if len(core.sambaCalls) != 1 || core.sambaCalls[0] != "add alice secret:correct horse" {
		t.Errorf("core calls = %v", core.sambaCalls)
	}

	// A short password is core's call, and it must surface as a client error
	// rather than a 500.
	core.sambaErr = status.Error(codes.InvalidArgument, "password must be at least 8 characters")
	code, _, text = getJSON(t, s, http.MethodPost, "/api/v1/users/"+id+"/smb-password",
		`{"password":"short"}`, s.handleSetSambaPassword, "id", id)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (%s)", code, text)
	}
	core.sambaErr = nil

	// Removing the account stops SMB accepting the name at all.
	core.sambaCalls = nil
	code, decoded, text = getJSON(t, s, http.MethodDelete, "/api/v1/users/"+id+"/smb-password", "", s.handleDeleteSambaPassword, "id", id)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if decoded["provisioned"] != false {
		t.Errorf("provisioned = %v, want false after a removal", decoded["provisioned"])
	}
	if len(core.sambaCalls) != 1 || core.sambaCalls[0] != "remove alice secret:" {
		t.Errorf("core calls = %v", core.sambaCalls)
	}

	// An unknown user is a 404 before core is asked anything.
	core.sambaCalls = nil
	code, _, _ = getJSON(t, s, http.MethodPost, "/api/v1/users/nope/smb-password", `{"password":"correct horse"}`, s.handleSetSambaPassword, "id", "nope")
	if code != http.StatusNotFound {
		t.Errorf("unknown user: status = %d, want 404", code)
	}
	if len(core.sambaCalls) != 0 {
		t.Errorf("core was asked about a user that does not exist: %v", core.sambaCalls)
	}
}

// The audit trail answers "why can this person not reach this share": the
// grantees and refusals, newest first, paged.
func TestAccessAuditPassesFiltersThrough(t *testing.T) {
	s, _, core, _ := accessFixture(t)
	core.events = []*onyxv1.AccessEvent{{Id: 1, Kind: "grant", Share: "media", Username: "dana", Mode: "read", Actor: "ada"}}

	code, decoded, text := getJSON(t, s, http.MethodGet, "/api/v1/audit/access?share=media&limit=10", "", s.handleAccessAudit)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if core.eventReq.GetShare() != "media" || core.eventReq.GetLimit() != 10 {
		t.Errorf("core saw share=%q limit=%d", core.eventReq.GetShare(), core.eventReq.GetLimit())
	}
	events, _ := decoded["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v", decoded["events"])
	}
	if first, _ := events[0].(map[string]any); first["kind"] != "grant" {
		t.Errorf("event = %v", events[0])
	}

	// No limit asked for means core's page size, not every event ever recorded.
	core.eventReq = nil
	if code, _, text := getJSON(t, s, http.MethodGet, "/api/v1/audit/access", "", s.handleAccessAudit); code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if core.eventReq.GetLimit() != 0 || core.eventReq.GetShare() != "" {
		t.Errorf("core saw share=%q limit=%d, want no filter", core.eventReq.GetShare(), core.eventReq.GetLimit())
	}

	// A malformed limit is refused rather than quietly ignored.
	for _, raw := range []string{"abc", "-1"} {
		if code, _, _ := getJSON(t, s, http.MethodGet, "/api/v1/audit/access?limit="+raw, "", s.handleAccessAudit); code != http.StatusBadRequest {
			t.Errorf("limit=%s: status = %d, want 400", raw, code)
		}
	}

	// An unreachable core is reported, not shown as an empty trail.
	core.eventErr = status.Error(codes.Unavailable, "onyx-core is not reachable")
	if code, _, _ := getJSON(t, s, http.MethodGet, "/api/v1/audit/access", "", s.handleAccessAudit); code == http.StatusOK {
		t.Error("an unreachable core must not look like an empty audit trail")
	}
}
