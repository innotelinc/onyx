package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// fakeCoreShares is an in-memory CoreShares client: enough of onyx-core's share
// and grant behaviour for the gateway's permission endpoints to be tested
// against something that actually stores what they write.
type fakeCoreShares struct {
	grants map[string]string // "share\x00user" -> mode
	calls  []string          // "set <share> <user> <mode>" in order
	actors map[string]string // grant key -> the actor that set it
}

func newFakeCoreShares() *fakeCoreShares {
	return &fakeCoreShares{grants: map[string]string{}, actors: map[string]string{}}
}

func grantKey(share, user string) string { return share + "\x00" + user }

func (f *fakeCoreShares) CreateShare(context.Context, *onyxv1.CreateShareRequest, ...grpc.CallOption) (*onyxv1.Share, error) {
	return nil, http.ErrNotSupported
}

func (f *fakeCoreShares) ListShares(context.Context, *onyxv1.ListSharesRequest, ...grpc.CallOption) (*onyxv1.ListSharesResponse, error) {
	return &onyxv1.ListSharesResponse{}, nil
}

func (f *fakeCoreShares) GetShare(context.Context, *onyxv1.GetShareRequest, ...grpc.CallOption) (*onyxv1.Share, error) {
	return nil, http.ErrNotSupported
}

func (f *fakeCoreShares) DeleteShare(context.Context, *onyxv1.DeleteShareRequest, ...grpc.CallOption) (*onyxv1.DeleteShareResponse, error) {
	return &onyxv1.DeleteShareResponse{}, nil
}

func (f *fakeCoreShares) SetShareAccess(_ context.Context, in *onyxv1.SetShareAccessRequest, _ ...grpc.CallOption) (*onyxv1.SetShareAccessResponse, error) {
	a := in.GetAccess()
	f.calls = append(f.calls, "set "+a.GetShare()+" "+a.GetUsername()+" "+a.GetMode())
	if a.GetMode() == "" {
		delete(f.grants, grantKey(a.GetShare(), a.GetUsername()))
	} else {
		f.grants[grantKey(a.GetShare(), a.GetUsername())] = a.GetMode()
	}
	f.actors[grantKey(a.GetShare(), a.GetUsername())] = in.GetActor()
	return &onyxv1.SetShareAccessResponse{Access: a}, nil
}

func (f *fakeCoreShares) ListShareAccess(_ context.Context, in *onyxv1.ListShareAccessRequest, _ ...grpc.CallOption) (*onyxv1.ListShareAccessResponse, error) {
	resp := &onyxv1.ListShareAccessResponse{}
	for key, mode := range f.grants {
		share, user, _ := strings.Cut(key, "\x00")
		if in.GetShare() != "" && in.GetShare() != share {
			continue
		}
		if in.GetUsername() != "" && in.GetUsername() != user {
			continue
		}
		resp.Access = append(resp.Access, &onyxv1.ShareAccess{Share: share, Username: user, Mode: mode})
	}
	return resp, nil
}

// permissionsFor is the Users-page read: the page must see what the backends
// are rendered from, so this asserts the gateway asks core rather than a copy.
func permissionsFor(t *testing.T, s *server, id string) map[string]string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/"+id+"/permissions", nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleUserPermissions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Permissions map[string]string `json:"permissions"`
		Username    string            `json:"username"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Permissions
}

func putPermissions(t *testing.T, s *server, id, payload string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/"+id+"/permissions", strings.NewReader(payload))
	req.SetPathValue("id", id)
	s.handleSetUserPermissions(rec, req)
	return rec
}

func newPermissionsServer(t *testing.T) (*server, *fakeCoreShares) {
	t.Helper()
	store, err := newUserStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUserStore: %v", err)
	}
	if _, err := store.upsert("alice", func(*onyxUser) {}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	fake := newFakeCoreShares()
	return &server{users: store, coreShares: fake}, fake
}

// A save that changes nothing must not rewrite the backends: every write
// re-renders the daemon config, so an unchanged panel save has to be quiet.
func TestSetPermissionsWritesOnlyTheDiff(t *testing.T) {
	s, fake := newPermissionsServer(t)
	id := s.users.list()[0].ID

	if rec := putPermissions(t, s, id, `{"permissions":{"media":"read"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := fake.grants[grantKey("media", "alice")]; got != "read" {
		t.Fatalf("grant = %q, want read", got)
	}

	// The same map again, plus an unchanged second share, writes nothing new.
	fake.calls = nil
	if rec := putPermissions(t, s, id, `{"permissions":{"media":"read","docs":"read-write"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(fake.calls) != 1 || fake.calls[0] != "set docs alice read-write" {
		t.Errorf("calls = %v, want only the new grant", fake.calls)
	}

	// Reads report what core holds, so the panel and the daemons agree.
	perms := permissionsFor(t, s, id)
	if perms["media"] != "read" || perms["docs"] != "read-write" {
		t.Errorf("permissions = %v", perms)
	}
}

// The body is the whole desired map: a share left out is a grant to remove,
// which is what the panel does when somebody is set back to "No access".
func TestSetPermissionsRemovesOmittedShares(t *testing.T) {
	s, fake := newPermissionsServer(t)
	id := s.users.list()[0].ID

	if rec := putPermissions(t, s, id, `{"permissions":{"media":"read","docs":"read-write"}}`); rec.Code != http.StatusOK {
		t.Fatalf("seed: status = %d", rec.Code)
	}
	if rec := putPermissions(t, s, id, `{"permissions":{"docs":"read-write"}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := fake.grants[grantKey("media", "alice")]; ok {
		t.Error("media grant survived being left out of the map")
	}
	// An explicit empty mode removes too, matching the panel's "No access".
	if rec := putPermissions(t, s, id, `{"permissions":{"docs":""}}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(fake.grants) != 0 {
		t.Errorf("grants after clearing = %v, want none", fake.grants)
	}
}

// A mode the renderers cannot express is refused before anything is written,
// rather than stored and rendered as nothing.
func TestSetPermissionsRejectsUnknownMode(t *testing.T) {
	s, fake := newPermissionsServer(t)
	id := s.users.list()[0].ID

	rec := putPermissions(t, s, id, `{"permissions":{"media":"owner"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if len(fake.calls) != 0 {
		t.Errorf("an invalid mode reached core: %v", fake.calls)
	}
}

// Deleting the mapping clears the grants too: a name left in a share's access
// list would be a permission the console no longer shows.
func TestDeleteUserClearsShareGrants(t *testing.T) {
	s, fake := newPermissionsServer(t)
	user := s.users.list()[0]
	id := user.ID
	if _, err := fake.SetShareAccess(context.Background(), &onyxv1.SetShareAccessRequest{
		Access: &onyxv1.ShareAccess{Share: "media", Username: user.Username, Mode: "read"},
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	s.handleDeleteUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(fake.grants) != 0 {
		t.Errorf("grants survived the delete: %v", fake.grants)
	}
	if !strings.Contains(rec.Body.String(), `"grants_removed":1`) {
		t.Errorf("response should report the removal: %s", rec.Body.String())
	}
}
