package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A console smoke test: the whole HTTP surface the browser talks to, booted the
// way main() boots it (registerRoutes) against stubbed upstreams, and driven the
// way the pages drive it. The unit tests around each handler prove the handler;
// this proves the routes, the wiring and the round trip a person actually makes
// — add a user, sign a cloud account in, and set who can reach a share.

// fakeRcloneDriver is a stub rclone that answers the two calls the OAuth
// sign-in makes (a config dump to read the remote's OAuth app, a config update
// to store the token) and logs every argv it is given. Whatever it is asked to
// do, it succeeds, so the test observes the console's behaviour rather than a
// provider's.
func fakeRcloneDriver(t *testing.T, configJSON string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rclone.args")
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(configJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
		"if [ \"$1\" = config ] && [ \"$2\" = dump ]; then cat '" + cfgPath + "'; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// consoleFixture boots the API's real router over a stubbed identity provider,
// stubbed share backend and stubbed rclone, and returns an HTTP client for it.
type consoleFixture struct {
	*httptest.Server
	authentik *fakeAuthentik
	shares    *fakeCoreShares
	// rcloneLog is the file the stub appends every argv to.
	rcloneLog string
}

func newConsoleFixture(t *testing.T) *consoleFixture {
	t.Helper()
	authentik := newFakeAuthentik(t)
	shares := newFakeCoreShares()
	rcloneLog := fakeRcloneDriver(t, `{"onyx-drive":{"type":"drive","client_id":"cid","client_secret":"csecret"}}`)

	store, err := newUserStore(t.TempDir())
	if err != nil {
		t.Fatalf("newUserStore: %v", err)
	}
	t.Setenv("AUTHENTIK_URL", authentik.URL)
	t.Setenv("AUTHENTIK_TOKEN", "test-token")
	t.Setenv("ONYX_PUBLIC_URL", "")

	s := &server{users: store, authentik: authentikFromEnv(), coreShares: shares}
	s.registerRoutes()

	fixture := &consoleFixture{authentik: authentik, shares: shares, rcloneLog: rcloneLog}
	fixture.Server = httptest.NewServer(s)
	t.Cleanup(fixture.Close)
	return fixture
}

// doJSON performs one request against the booted console and decodes the body.
func (f *consoleFixture) doJSON(t *testing.T, method, path, body string) (int, map[string]any, string) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(raw)
	decoded := map[string]any{}
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			t.Fatalf("%s %s returned unreadable JSON: %v\n%s", method, path, err, text)
		}
	}
	return resp.StatusCode, decoded, text
}

// The three console journeys, end to end over HTTP: a user who exists in both
// stores, a cloud account that finishes signing in, and a share grant that the
// backends will enforce.
func TestConsoleSmokeJourneys(t *testing.T) {
	f := newConsoleFixture(t)

	// --- 1. Adding a user here adds the identity in Authentik ---
	status, created, text := f.doJSON(t, http.MethodPost, "/api/v1/users", `{"username":"alex","display_name":"Alex Doe","role":"user"}`)
	if status != http.StatusCreated {
		t.Fatalf("create user: status = %d (%s)", status, text)
	}
	if len(f.authentik.created) != 1 || f.authentik.created[0]["username"] != "alex" {
		t.Errorf("the console stored a mapping without creating the identity: %v", f.authentik.created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create user returned no id: %s", text)
	}

	// The same page shows the Authentik half: an account that only exists in
	// the provider is listed too, with a role assigned on sight.
	f.authentik.users = append(f.authentik.users, authentikUser{PK: 42, Username: "sam", Name: "Sam Roe", IsActive: true})
	status, list, text := f.doJSON(t, http.MethodGet, "/api/v1/users", "")
	if status != http.StatusOK {
		t.Fatalf("list users: status = %d (%s)", status, text)
	}
	users, _ := list["users"].([]any)
	var sawAlex, sawSam bool
	for _, entry := range users {
		record, _ := entry.(map[string]any)
		switch record["username"] {
		case "alex":
			sawAlex = true
		case "sam":
			sawSam = true
			if record["role"] != "user" {
				t.Errorf("an Authentik account got no Onyx role: %v", record)
			}
		}
	}
	if !sawAlex || !sawSam {
		t.Errorf("the merged list is missing a user (alex=%v sam=%v): %s", sawAlex, sawSam, text)
	}

	// --- 2. A cloud account signs in through the console ---
	status, start, text := f.doJSON(t, http.MethodPost, "/api/v1/storage/remotes/onyx-drive/oauth/start", "")
	if status != http.StatusOK {
		t.Fatalf("oauth start: status = %d (%s)", status, text)
	}
	authURL, _ := start["auth_url"].(string)
	if authURL == "" {
		t.Fatalf("oauth start returned no auth_url: %s", text)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("auth_url is not a URL: %v", err)
	}
	query := parsed.Query()
	if query.Get("client_id") != "cid" {
		t.Errorf("the remote's own OAuth app was not used: %s", authURL)
	}
	if !strings.HasSuffix(query.Get("redirect_uri"), callbackPath) {
		t.Errorf("redirect_uri = %q, want it to end with %s", query.Get("redirect_uri"), callbackPath)
	}
	state := query.Get("state")
	if state == "" {
		t.Fatalf("auth_url carries no state, so the callback could not be bound to the remote: %s", authURL)
	}

	// The provider's side of the exchange is stubbed; everything after it (the
	// token rclone is handed, the probe) is the console's own work.
	original := exchangeOAuthCodeFunc
	exchangeOAuthCodeFunc = func(context.Context, oauthBackend, string, string, string, string) (oauthTokenResponse, error) {
		return oauthTokenResponse{AccessToken: "ya29.smoke", RefreshToken: "1//smoke", TokenType: "Bearer", ExpiresIn: 3600}, nil
	}
	t.Cleanup(func() { exchangeOAuthCodeFunc = original })

	status, _, page := f.doJSON(t, http.MethodGet, callbackPath+"?state="+url.QueryEscape(state)+"&code=smoke-code", "")
	if status != http.StatusOK || !strings.Contains(page, "is connected") {
		t.Fatalf("callback: status = %d, page = %s", status, page)
	}
	logged, err := os.ReadFile(f.rcloneLog)
	if err != nil {
		t.Fatalf("fake rclone was never invoked: %v", err)
	}
	calls := string(logged)
	if !strings.Contains(calls, "config update onyx-drive token") || !strings.Contains(calls, "ya29.smoke") {
		t.Errorf("the provider's token never reached rclone:\n%s", calls)
	}
	if !strings.Contains(calls, "--non-interactive") {
		t.Errorf("storing the token must not start an interactive flow:\n%s", calls)
	}
	if !strings.Contains(calls, "lsd onyx-drive:") {
		t.Errorf("the account was never probed after signing in:\n%s", calls)
	}

	// A state is single-use: replaying the redirect must not store a second
	// token (or silently re-authorize somebody else's link).
	status, _, page = f.doJSON(t, http.MethodGet, callbackPath+"?state="+url.QueryEscape(state)+"&code=smoke-code", "")
	if status == http.StatusOK {
		t.Errorf("a replayed callback was accepted: %s", page)
	}
	if strings.Contains(page, "is connected") {
		t.Errorf("the replayed callback reported success: %s", page)
	}

	// A refusal from the provider is explained, not treated as success.
	status, _, page = f.doJSON(t, http.MethodGet, callbackPath+"?error=access_denied&error_description=The+user+refused", "")
	if status != http.StatusBadRequest || !strings.Contains(page, "refused") {
		t.Errorf("provider refusal: status = %d, page = %s", status, page)
	}

	// --- 3. Share access is written where the backends read it ---
	status, _, text = f.doJSON(t, http.MethodPut, "/api/v1/users/"+id+"/permissions",
		`{"permissions":{"media":"read-write"}}`)
	if status != http.StatusOK {
		t.Fatalf("set permissions: status = %d (%s)", status, text)
	}
	if got := f.shares.grants[grantKey("media", "alex")]; got != "read-write" {
		t.Errorf("the grant did not reach the share backend's record (core): %q", got)
	}
	status, perms, text := f.doJSON(t, http.MethodGet, "/api/v1/users/"+id+"/permissions", "")
	if status != http.StatusOK {
		t.Fatalf("get permissions: status = %d (%s)", status, text)
	}
	got, _ := perms["permissions"].(map[string]any)
	if got["media"] != "read-write" {
		t.Errorf("the panel reads back %v, want media=read-write", perms["permissions"])
	}

	// Deleting the mapping clears the grant: a disabled identity must not keep
	// a share open.
	status, _, text = f.doJSON(t, http.MethodDelete, "/api/v1/users/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("delete user: status = %d (%s)", status, text)
	}
	if len(f.shares.grants) != 0 {
		t.Errorf("grants survived the account being removed: %v", f.shares.grants)
	}
}
