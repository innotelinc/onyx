package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Identity lives in Authentik and the role lives in Onyx, so the Users page has
// to present one user out of two records — and adding or removing one has to
// touch both stores or the console tells a story that is not true.

// fakeAuthentik serves the handful of core-API endpoints the sync uses and
// records the writes it received.
type fakeAuthentik struct {
	*httptest.Server
	users    []authentikUser
	created  []map[string]any
	patched  []map[string]any
	deleted  []string
	failNext int
}

func newFakeAuthentik(t *testing.T, users ...authentikUser) *fakeAuthentik {
	t.Helper()
	fake := &fakeAuthentik{users: users}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/core/users/", func(w http.ResponseWriter, r *http.Request) {
		want := r.URL.Query().Get("username")
		results := []authentikUser{}
		for _, u := range fake.users {
			if want == "" || strings.EqualFold(u.Username, want) {
				results = append(results, u)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"pagination": map[string]any{"next": "", "count": len(results), "current_page": 1, "total_pages": 1},
			"results":    results,
		})
	})
	mux.HandleFunc("POST /api/v3/core/users/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fake.created = append(fake.created, body)
		if fake.failNext > 0 {
			fake.failNext--
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"nope"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(authentikUser{PK: 99, Username: body["username"].(string), IsActive: true})
	})
	mux.HandleFunc("PATCH /api/v3/core/users/{pk}/", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["pk"] = r.PathValue("pk")
		fake.patched = append(fake.patched, body)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("DELETE /api/v3/core/users/{pk}/", func(w http.ResponseWriter, r *http.Request) {
		fake.deleted = append(fake.deleted, r.PathValue("pk"))
		w.WriteHeader(http.StatusNoContent)
	})
	fake.Server = httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	return fake
}

// testServerWithAuthentik builds a server whose identity provider is the fake.
func testServerWithAuthentik(t *testing.T, fake *fakeAuthentik) *server {
	t.Helper()
	t.Setenv("AUTHENTIK_URL", fake.URL)
	t.Setenv("AUTHENTIK_TOKEN", "test-token")
	store, err := newUserStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUserStore: %v", err)
	}
	return &server{users: store, authentik: authentikFromEnv()}
}

func TestUsersMergesAuthentikAccountsWithOnyxRoles(t *testing.T) {
	fake := newFakeAuthentik(t,
		authentikUser{PK: 1, Username: "alex", Name: "Alex Doe", Email: "alex@example.com", IsActive: true},
		authentikUser{PK: 2, Username: "sam", Name: "Sam Roe", IsActive: false},
	)
	s := testServerWithAuthentik(t, fake)
	// Alex already has an Onyx role; that role must survive the merge.
	if _, err := s.users.upsert("alex", func(u *onyxUser) { u.Role = "admin" }); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleUsers(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Users []onyxUser `json:"users"`
		Sync  struct {
			Enabled bool `json:"enabled"`
			Count   int  `json:"authentik_users"`
		} `json:"authentik"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Sync.Enabled || body.Sync.Count != 2 {
		t.Errorf("sync = %+v, want Authentik reported as the source", body.Sync)
	}
	if len(body.Users) != 2 {
		t.Fatalf("got %d users (%+v), want one per Authentik account", len(body.Users), body.Users)
	}
	byName := map[string]onyxUser{}
	for _, u := range body.Users {
		byName[u.Username] = u
	}
	alex := byName["alex"]
	if alex.Role != "admin" {
		t.Errorf("alex's Onyx role = %q, want it preserved", alex.Role)
	}
	if alex.Source != "authentik+onyx" || !alex.InAuthentik || !alex.Active {
		t.Errorf("alex = %+v, want the merge of both stores", alex)
	}
	// An Authentik account with no Onyx role gets one on first sight — this is
	// the automatic mapping, and a disabled account is mapped as locked rather
	// than silently active.
	sam := byName["sam"]
	if sam.Role != "user" {
		t.Errorf("sam's auto-mapped role = %q, want user", sam.Role)
	}
	if sam.Status != "locked" || sam.Active {
		t.Errorf("sam = %+v, want locked because Authentik has the account disabled", sam)
	}
	// The mapping is persisted, so the next read is stable rather than a second
	// wave of writes.
	rec = httptest.NewRecorder()
	s.handleUsers(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users", nil))
	var again struct {
		Users []onyxUser `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, u := range again.Users {
		if u.Username == "sam" && u.Role != "user" {
			t.Errorf("the auto-mapping did not persist: %+v", u)
		}
	}
}

func TestUsersReportsWhySyncIsOff(t *testing.T) {
	t.Setenv("AUTHENTIK_URL", "")
	t.Setenv("AUTHENTIK_TOKEN", "")
	store, err := newUserStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUserStore: %v", err)
	}
	s := &server{users: store, authentik: authentikFromEnv()}
	rec := httptest.NewRecorder()
	s.handleUsers(rec, httptest.NewRequest(http.MethodGet, "/api/v1/users", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AUTHENTIK_URL") {
		t.Errorf("the console is not told why there is no Authentik sync: %s", rec.Body.String())
	}
}

func TestCreateUserCreatesTheAuthentikAccountFirst(t *testing.T) {
	fake := newFakeAuthentik(t)
	s := testServerWithAuthentik(t, fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"username":"alex","email":"alex@example.com","role":"operator","status":"active"}`))
	s.handleCreateUser(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if len(fake.created) != 1 {
		t.Fatalf("Authentik received %d creates, want 1", len(fake.created))
	}
	if fake.created[0]["username"] != "alex" || fake.created[0]["is_active"] != true {
		t.Errorf("Authentik was asked to create %+v", fake.created[0])
	}
	// The account must have no password set from here: Authentik's recovery flow
	// is the only place a password should be minted.
	if _, ok := fake.created[0]["password"]; ok {
		t.Errorf("a password was set through the API: %+v", fake.created[0])
	}
	if _, ok := s.getUserByUsername("alex"); !ok {
		t.Error("the Onyx role mapping was not stored")
	}
}

func TestCreateUserFailsWhenAuthentikRefuses(t *testing.T) {
	fake := newFakeAuthentik(t)
	fake.failNext = 1
	s := testServerWithAuthentik(t, fake)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(`{"username":"alex","role":"user"}`))
	s.handleCreateUser(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.getUserByUsername("alex"); ok {
		t.Error("an Onyx mapping was stored for an account Authentik refused: it would list a user who cannot sign in")
	}
}

func TestDeleteUserDisablesTheIdentityAndPurgeErasesIt(t *testing.T) {
	fake := newFakeAuthentik(t, authentikUser{PK: 7, Username: "alex", IsActive: true})
	s := testServerWithAuthentik(t, fake)
	created, err := s.users.upsert("alex", func(u *onyxUser) { u.Role = "user" })
	if err != nil {
		t.Fatalf("seed mapping: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+created.ID, nil)
	req.SetPathValue("id", created.ID)
	rec := httptest.NewRecorder()
	s.handleDeleteUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(fake.patched) != 1 || fake.patched[0]["is_active"] != false {
		t.Errorf("removing a user should disable the identity, saw %+v", fake.patched)
	}
	if len(fake.deleted) != 0 {
		t.Errorf("removing a user erased the identity instead of disabling it: %v", fake.deleted)
	}
	if _, ok := s.getUser(created.ID); ok {
		t.Error("the Onyx role mapping survived removal")
	}
	// A purge is the deliberate erase-everywhere path.
	second, err := s.users.upsert("alex", func(u *onyxUser) { u.Role = "user" })
	if err != nil {
		t.Fatalf("re-seed mapping: %v", err)
	}
	purgeReq := httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+second.ID+"?purge=true", nil)
	purgeReq.SetPathValue("id", second.ID)
	purgeRec := httptest.NewRecorder()
	s.handleDeleteUser(purgeRec, purgeReq)
	if purgeRec.Code != http.StatusOK {
		t.Fatalf("purge status = %d, want 200: %s", purgeRec.Code, purgeRec.Body.String())
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "7" {
		t.Errorf("purge did not erase the Authentik account: %v", fake.deleted)
	}
}
