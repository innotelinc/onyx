package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// renderedConf mirrors the exact shape onyx-shared emits for two shares, so the
// parser is tested against the real wire format rather than a simplified one.
const renderedConf = `# Onyx-generated WebDAV configuration for onyx-davd.
# Managed by onyx-shared / onyx-core; do not edit by hand. (docs/design/05#6)

[server]
listen = "127.0.0.1:8081"
tls = "upstream"
auth = "gateway"

# One [[share]] table per WebDAV-enabled share.

[[share]]
name = "media"
path = "/mnt/onyx/main-pool/media"
readonly = false
allowed_users = []
readonly_users = []

[[share]]
name = "archive"
path = "/mnt/onyx/main-pool/archive"
readonly = true
allowed_users = []
readonly_users = []
`

func TestParseConfigRenderedShape(t *testing.T) {
	cfg, err := parseConfig([]byte(renderedConf))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8081" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.Auth != authModeGateway {
		t.Errorf("auth = %q", cfg.Auth)
	}
	if cfg.IdentityHeader != "X-Onyx-User" {
		t.Errorf("identity header = %q", cfg.IdentityHeader)
	}
	if len(cfg.Shares) != 2 {
		t.Fatalf("shares = %d, want 2", len(cfg.Shares))
	}
	if cfg.Shares[0].Name != "media" || cfg.Shares[0].Path != "/mnt/onyx/main-pool/media" || cfg.Shares[0].Readonly {
		t.Errorf("share[0] = %+v", cfg.Shares[0])
	}
	if !cfg.Shares[1].Readonly {
		t.Errorf("share[1] should be read-only: %+v", cfg.Shares[1])
	}
	// The renderer always emits the grant lists; empty means no restriction.
	if len(cfg.Shares[0].AllowedUsers) != 0 || len(cfg.Shares[0].ReadonlyUsers) != 0 {
		t.Errorf("share[0] grants = %v / %v, want none", cfg.Shares[0].AllowedUsers, cfg.Shares[0].ReadonlyUsers)
	}
}

func TestParseConfigEmptyIsUsable(t *testing.T) {
	// A render with no WebDAV shares still has to load: the daemon runs with an
	// empty share table rather than failing to start.
	cfg, err := parseConfig([]byte("[server]\nlisten = \"127.0.0.1:8081\"\n"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Shares) != 0 {
		t.Fatalf("shares = %d, want 0", len(cfg.Shares))
	}
}

func TestParseConfigRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"unknown server key":  "[server]\nlisten = \"127.0.0.1:8081\"\nwat = \"1\"\n",
		"unknown share key":   "[[share]]\nname = \"media\"\npath = \"/mnt/onyx/media\"\nwat = \"1\"\n",
		"unquoted value":      "[server]\nlisten = 127.0.0.1:8081\n",
		"key before section":  "listen = \"127.0.0.1:8081\"\n",
		"relative share path": "[[share]]\nname = \"media\"\npath = \"media\"\n",
		"dirty share path":    "[[share]]\nname = \"media\"\npath = \"/mnt/onyx/../etc\"\n",
		"bad share name":      "[[share]]\nname = \"Media Share\"\npath = \"/mnt/onyx/media\"\n",
		"duplicate name":      "[[share]]\nname = \"media\"\npath = \"/mnt/onyx/a\"\n[[share]]\nname = \"media\"\npath = \"/mnt/onyx/b\"\n",
		"unknown auth":        "[server]\nauth = \"trust-me\"\n",
		"listen without port": "[server]\nlisten = \"127.0.0.1\"\n",
		"unsupported section": "[dav]\nlisten = \"127.0.0.1:8081\"\n",
		// A grant list has to be a list: a bare string would parse as nothing
		// and read as "no restriction", the opposite of what it says.
		"scalar user list": "[[share]]\nname = \"media\"\npath = \"/mnt/onyx/media\"\nallowed_users = \"alice\"\n",
		"invalid grantee":  "[[share]]\nname = \"media\"\npath = \"/mnt/onyx/media\"\nallowed_users = [\"a b\"]\n",
		"invalid readonly": "[[share]]\nname = \"media\"\npath = \"/mnt/onyx/media\"\nreadonly_users = [\"\"]\n",
	}
	for name, input := range cases {
		if _, err := parseConfig([]byte(input)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

// auth = "none" is loopback-only: the listener is the whole security boundary,
// so a routable bind address must be refused.
func TestParseConfigAuthNoneRequiresLoopback(t *testing.T) {
	if _, err := parseConfig([]byte("[server]\nauth = \"none\"\nlisten = \"0.0.0.0:8081\"\n")); err == nil {
		t.Error("auth=none on a routable address: expected an error, got none")
	}
	if _, err := parseConfig([]byte("[server]\nauth = \"none\"\nlisten = \"127.0.0.1:8081\"\n")); err != nil {
		t.Errorf("auth=none on loopback: %v", err)
	}
}

// --listen overrides the config's address, so it is validated against the
// config's own auth mode: without this, a loopback-only (auth = "none") config
// could be started on a routable address where the listener is the only
// boundary.
func TestApplyListenOverrideRevalidates(t *testing.T) {
	cfg, err := parseConfig([]byte("[server]\nauth = " + `"none"` + "\nlisten = \"127.0.0.1:8081\"\n"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if err := applyListenOverride(cfg, "0.0.0.0:8081"); err == nil {
		t.Error("auth=none with a routable override: expected an error, got none")
	}
	if cfg.Listen != "0.0.0.0:8081" {
		t.Errorf("override not applied: listen = %q", cfg.Listen)
	}

	cfg, err = parseConfig([]byte("[server]\nauth = " + `"gateway"` + "\nlisten = \"127.0.0.1:8081\"\n"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if err := applyListenOverride(cfg, "0.0.0.0:8081"); err != nil {
		t.Errorf("gateway auth with a routable override: %v", err)
	}

	// An absent override leaves the configured address alone.
	cfg, _ = parseConfig([]byte("[server]\nlisten = \"127.0.0.1:8081\"\n"))
	if err := applyListenOverride(cfg, ""); err != nil || cfg.Listen != "127.0.0.1:8081" {
		t.Errorf("empty override changed listen: %q, %v", cfg.Listen, err)
	}
}

func TestStripCommentKeepsHashInsideQuotes(t *testing.T) {
	got := stripComment(`	path = "/mnt/onyx/pool#1/media" # trailing`)
	want := `	path = "/mnt/onyx/pool#1/media" `
	if got != want {
		t.Errorf("stripComment = %q, want %q", got, want)
	}
}

// --- HTTP surface ---

func newTestServer(t *testing.T, conf string) http.Handler {
	t.Helper()
	cfg, err := parseConfig([]byte(conf))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return newHandler(cfg)
}

func TestGatewayAuthIsRequired(t *testing.T) {
	h := newTestServer(t, shareConf(t.TempDir(), false))

	req := httptest.NewRequest(http.MethodGet, "/webdav/media/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no identity header: status = %d, want 401", rec.Code)
	}

	// PROPFIND is how a WebDAV client opens a collection; GET on a collection
	// is not a WebDAV operation and the library answers 405 by design.
	req = httptest.NewRequest("PROPFIND", "/webdav/media/", nil)
	req.Header.Set("X-Onyx-User", "alice")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("with identity header: status = %d, want 207 (%s)", rec.Code, rec.Body.String())
	}
}

func TestIndexListsShares(t *testing.T) {
	h := newTestServer(t, renderedConf)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Onyx-User", "alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"media"`) || !strings.Contains(body, `"archive"`) {
		t.Errorf("index missing shares: %s", body)
	}
}

func TestWebdavRoundTripOnWritableShare(t *testing.T) {
	root := t.TempDir()
	h := newTestServer(t, shareConf(root, false))

	// MKCOL then PUT then GET, the sequence a sync client performs.
	rec := do(t, h, "MKCOL", "/webdav/media/docs", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("MKCOL status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPut, "/webdav/media/docs/note.txt", strings.NewReader("hello"))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 2xx (%s)", rec.Code, rec.Body.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "docs", "note.txt"))
	if err != nil {
		t.Fatalf("file not written into the share: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("file contents = %q, want %q", got, "hello")
	}
	rec = do(t, h, http.MethodGet, "/webdav/media/docs/note.txt", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("GET status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestReadonlyShareRejectsWrites(t *testing.T) {
	root := t.TempDir()
	h := newTestServer(t, shareConf(root, true))

	rec := do(t, h, http.MethodPut, "/webdav/media/note.txt", strings.NewReader("nope"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT on readonly share: status = %d, want 403", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(root, "note.txt")); !os.IsNotExist(err) {
		t.Error("readonly share accepted a write")
	}
	// Reads still work: read-only is a write policy, not a denial of the share.
	if rec := do(t, h, "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND on readonly share: status = %d, want 207", rec.Code)
	}
}

// GET on a collection is answered with 405 by the WebDAV library (collections
// are read with PROPFIND), which is the behaviour clients expect.
func TestGetOnCollectionIsMethodNotAllowed(t *testing.T) {
	h := newTestServer(t, shareConf(t.TempDir(), false))
	if rec := do(t, h, http.MethodGet, "/webdav/media/", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestSharePathWithoutTrailingSlashRedirects(t *testing.T) {
	h := newTestServer(t, shareConf(t.TempDir(), false))
	rec := do(t, h, http.MethodGet, "/webdav/media", nil)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/webdav/media/" {
		t.Errorf("Location = %q, want /webdav/media/", loc)
	}
}

func TestUnknownShareIsNotMounted(t *testing.T) {
	h := newTestServer(t, shareConf(t.TempDir(), false))
	if rec := do(t, h, http.MethodGet, "/webdav/other/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// A share directory that is not mounted (or was removed) must answer 404, not
// 500: the config and the filesystem are allowed to disagree briefly.
func TestMissingSharePathIsNotFound(t *testing.T) {
	h := newTestServer(t, shareConf(filepath.Join(t.TempDir(), "gone"), false))
	if rec := do(t, h, http.MethodGet, "/webdav/media/", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// do performs one authenticated request against the handler.
func do(t *testing.T, h http.Handler, method, target string, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, body)
	}
	req.Header.Set("X-Onyx-User", "alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func shareConf(root string, readonly bool) string {
	return "[server]\nlisten = \"127.0.0.1:8081\"\nauth = \"gateway\"\n\n" +
		"[[share]]\nname = \"media\"\npath = " + `"` + root + `"` + "\nreadonly = " + boolStr(readonly) + "\n"
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func confWithGrants(root, allowed, readonly string) string {
	return "[server]\nlisten = \"127.0.0.1:8081\"\nauth = \"gateway\"\n\n" +
		"[[share]]\nname = \"media\"\npath = " + `"` + root + `"` + "\nreadonly = false\n" +
		"allowed_users = " + allowed + "\nreadonly_users = " + readonly + "\n"
}

// The grants the Access panel writes are enforced here, against the identity
// the gateway passed: a name outside the list is refused the share entirely,
// and a `read` grant cannot write even on a read-write share. Without this the
// panel would only change what the page says.
func TestGrantsRestrictWebdavAccess(t *testing.T) {
	root := t.TempDir()
	h := newTestServer(t, confWithGrants(root, `["alice"]`, `["alice"]`))

	// The grantee may read, but the grant says read-only, so a write is 403.
	if rec := doAs(t, h, "alice", "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusMultiStatus {
		t.Fatalf("grantee PROPFIND: status = %d, want 207 (%s)", rec.Code, rec.Body.String())
	}
	rec := doAs(t, h, "alice", http.MethodPut, "/webdav/media/note.txt", strings.NewReader("nope"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read grant accepted a write: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "read-only for alice") {
		t.Errorf("refusal should name the user and the reason: %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "note.txt")); !os.IsNotExist(err) {
		t.Error("a read grant wrote to the share")
	}

	// Somebody the share does not name gets nothing — not even the listing.
	if rec := doAs(t, h, "bob", "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted user PROPFIND: status = %d, want 403", rec.Code)
	}

	// The index is filtered the same way, so grants cannot be enumerated
	// around.
	rec = doAs(t, h, "bob", http.MethodGet, "/", nil)
	if strings.Contains(rec.Body.String(), `"media"`) {
		t.Errorf("index leaked a share bob cannot reach: %s", rec.Body.String())
	}
	rec = doAs(t, h, "alice", http.MethodGet, "/", nil)
	if !strings.Contains(rec.Body.String(), `"media"`) {
		t.Errorf("index hid a share alice can reach: %s", rec.Body.String())
	}
}

// A read-write grant still writes; the read-only rule belongs to the other
// user, not to the share.
func TestReadWriteGrantIsNotBlockedByAnothersReadGrant(t *testing.T) {
	root := t.TempDir()
	h := newTestServer(t, confWithGrants(root, `["alice","bob"]`, `["bob"]`))

	rec := doAs(t, h, "alice", http.MethodPut, "/webdav/media/note.txt", strings.NewReader("hello"))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("alice PUT: status = %d, want 2xx (%s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "note.txt")); err != nil {
		t.Fatalf("alice's write did not land: %v", err)
	}

	rec = doAs(t, h, "bob", http.MethodPut, "/webdav/media/other.txt", strings.NewReader("nope"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob PUT: status = %d, want 403", rec.Code)
	}
	if rec := doAs(t, h, "bob", "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusMultiStatus {
		t.Fatalf("bob PROPFIND: status = %d, want 207", rec.Code)
	}
}

// A container has no systemd to signal this daemon, so the config file is the
// trigger: the watcher has to notice a rewrite and swap the share table (grants
// included) without a restart. Without this, an access change written by the
// console would only take effect the next time the container was recreated.
func TestWatchConfigAppliesAChangeWithoutASignal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "davd.conf")
	root := t.TempDir()

	write := func(body string) {
		t.Helper()
		// Replace through a temp file + rename, the way onyx-privd writes it, so
		// the watcher sees a whole revision rather than a truncated one.
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	write(confWithGrants(root, `[]`, `[]`))

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	active := &reloadable{}
	active.swap(newHandler(cfg), cfg.Shares)
	go watchConfig(path, "", active, "", fingerprint(path), 2*time.Millisecond)

	if rec := doAs(t, active, "bob", "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusMultiStatus {
		t.Fatalf("before the change: status = %d, want 207", rec.Code)
	}

	write(confWithGrants(root, `["alice"]`, `["alice"]`))

	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := doAs(t, active, "bob", "PROPFIND", "/webdav/media/", nil)
		if rec.Code == http.StatusForbidden {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watcher never applied the new grants: status = %d", rec.Code)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The grantee is admitted, and read-only per the grant.
	if rec := doAs(t, active, "alice", "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusMultiStatus {
		t.Errorf("grantee after reload: status = %d, want 207", rec.Code)
	}
	if rec := doAs(t, active, "alice", http.MethodPut, "/webdav/media/x.txt", strings.NewReader("no")); rec.Code != http.StatusForbidden {
		t.Errorf("read grant after reload: status = %d, want 403", rec.Code)
	}
}

// "No grants" is the default and must keep behaving exactly as before: any
// authenticated identity reaches the share.
func TestNoGrantsKeepsTheShareOpenToAuthenticatedUsers(t *testing.T) {
	h := newTestServer(t, shareConf(t.TempDir(), false))
	if rec := doAs(t, h, "someone-else", "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", rec.Code)
	}
}

// A refusal is the moment the audit trail exists for, so each one is reported
// to onyx-core with enough to answer "why": the share, the identity, the method
// and whether the grant or the share's own mode caused it. A request that is
// allowed is never reported.
func TestDenialsAreReportedToTheAuditTrail(t *testing.T) {
	root := t.TempDir()
	h := newTestServer(t, confWithGrants(root, `["alice"]`, `["alice"]`))

	var reported []string
	original := recordDenial
	recordDenial = func(share, user, method, reason string) {
		reported = append(reported, share+" "+user+" "+method+" "+reason)
	}
	t.Cleanup(func() { recordDenial = original })

	if rec := doAs(t, h, "bob", "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("ungranted PROPFIND: status = %d, want 403", rec.Code)
	}
	if len(reported) != 1 || reported[0] != "media bob PROPFIND not granted" {
		t.Fatalf("reported = %v, want the ungranted refusal", reported)
	}

	// A `read` grant on a read-write share is a different cause, and the trail
	// has to tell them apart.
	reported = nil
	if rec := doAs(t, h, "alice", http.MethodPut, "/webdav/media/note.txt", strings.NewReader("nope")); rec.Code != http.StatusForbidden {
		t.Fatalf("read grant PUT: status = %d, want 403", rec.Code)
	}
	if len(reported) != 1 || reported[0] != "media alice PUT read-only grant" {
		t.Fatalf("reported = %v, want the read-grant refusal", reported)
	}

	// A request the grant allows is not a refusal, and recording it would bury
	// the real ones.
	reported = nil
	if rec := doAs(t, h, "alice", "PROPFIND", "/webdav/media/", nil); rec.Code != http.StatusMultiStatus {
		t.Fatalf("grantee PROPFIND: status = %d, want 207", rec.Code)
	}
	if len(reported) != 0 {
		t.Errorf("an allowed request was reported as a denial: %v", reported)
	}
}

// doAs is do for a named caller.
func doAs(t *testing.T, h http.Handler, user, method, target string, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, body)
	}
	req.Header.Set("X-Onyx-User", user)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
