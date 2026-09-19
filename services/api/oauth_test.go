package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The browser sign-in is the only way a Shares Google Drive / OneDrive account
// gets connected without the operator leaving the console, so the pieces that
// make it safe and finishable are pinned here: the redirect URI a provider has
// to have registered, the token rclone is given, and the state binding that
// keeps a callback from being replayed.

// fakeRcloneConfigured puts a stub rclone on PATH that logs every argv and
// answers `listremotes` and `config dump` from what the test says is configured,
// so no rclone install and no provider are involved.
func fakeRcloneConfigured(t *testing.T, listremotes, dump string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rclone.args")
	dumpPath := filepath.Join(dir, "dump.json")
	if err := os.WriteFile(dumpPath, []byte(dump), 0o644); err != nil {
		t.Fatalf("write config dump: %v", err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
		"if [ \"$1\" = listremotes ]; then printf '%s\\n' '" + listremotes + "'; exit 0; fi\n" +
		"if [ \"$1\" = config ] && [ \"$2\" = dump ]; then cat '" + dumpPath + "'; exit 0; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestOAuthCallbackURL(t *testing.T) {
	t.Setenv("ONYX_OAUTH_REDIRECT_URI", "")
	t.Setenv("ONYX_PUBLIC_URL", "")
	plain := httptest.NewRequest(http.MethodPost, "http://box.example/api/v1/storage/remotes/g/oauth/start", nil)
	if got := oauthCallbackURL(plain); got != "http://box.example"+callbackPath {
		t.Errorf("plain request = %q", got)
	}
	// Behind the ingress the request's own host is the proxy's internal
	// address, which the provider would never redirect back to — the forwarded
	// headers are what the browser actually used.
	proxied := httptest.NewRequest(http.MethodPost, "http://onyx-api:8080/api/v1/storage/remotes/g/oauth/start", nil)
	proxied.Host = "onyx-api:8080"
	proxied.Header.Set("X-Forwarded-Proto", "https")
	proxied.Header.Set("X-Forwarded-Host", "app.onyx.innotel.us")
	if got := oauthCallbackURL(proxied); got != "https://app.onyx.innotel.us"+callbackPath {
		t.Errorf("proxied request = %q", got)
	}
	// A deployment that pins the public URL wins over both.
	t.Setenv("ONYX_PUBLIC_URL", "https://onyx.example.com/")
	if got := oauthCallbackURL(plain); got != "https://onyx.example.com"+callbackPath {
		t.Errorf("ONYX_PUBLIC_URL = %q", got)
	}
	t.Setenv("ONYX_OAUTH_REDIRECT_URI", "https://custom.example/cb")
	if got := oauthCallbackURL(plain); got != "https://custom.example/cb" {
		t.Errorf("ONYX_OAUTH_REDIRECT_URI = %q", got)
	}
}

func TestRcloneTokenJSONMatchesRclonesShape(t *testing.T) {
	got, err := rcloneTokenJSON(oauthTokenResponse{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600})
	if err != nil {
		t.Fatalf("rcloneTokenJSON: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("the stored token is not JSON: %v", err)
	}
	if parsed["access_token"] != "at" || parsed["refresh_token"] != "rt" || parsed["token_type"] != "Bearer" {
		t.Errorf("token = %v, want the access/refresh/token_type rclone reads", parsed)
	}
	if _, ok := parsed["expiry"].(string); !ok {
		t.Errorf("token = %v, want an RFC3339 expiry so rclone refreshes before it lapses", parsed)
	}
	// A provider with no refresh token must not get an empty key: rclone reads
	// an empty string as a refresh token it can use.
	got, err = rcloneTokenJSON(oauthTokenResponse{AccessToken: "at"})
	if err != nil {
		t.Fatalf("rcloneTokenJSON: %v", err)
	}
	if strings.Contains(got, "refresh_token") || strings.Contains(got, "expiry") {
		t.Errorf("token = %s, want no refresh_token/expiry keys", got)
	}
}

func TestOAuthStateIsSingleUse(t *testing.T) {
	states := &oauthStates{states: map[string]pendingOAuth{}}
	state, err := states.put(pendingOAuth{remote: "gdrive", backendType: "drive"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, ok := states.take(state); !ok {
		t.Fatal("the state just issued was not found")
	}
	if _, ok := states.take(state); ok {
		t.Error("a replayed callback was accepted: a second token could be stored from one approval")
	}
	if _, ok := states.take("never-issued"); ok {
		t.Error("an unknown state was accepted")
	}
}

const gdriveConfigDump = `{"gdrive":{"type":"drive","client_id":"gid","client_secret":"gsecret"}}`

func TestOAuthStartBuildsTheProviderURL(t *testing.T) {
	fakeRcloneConfigured(t, "gdrive:", gdriveConfigDump)
	req := httptest.NewRequest(http.MethodPost, "https://box.example/api/v1/storage/remotes/gdrive/oauth/start", strings.NewReader("{}"))
	req.SetPathValue("name", "gdrive")
	rec := httptest.NewRecorder()
	(&server{}).handleOAuthStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AuthURL     string   `json:"auth_url"`
		RedirectURI string   `json:"redirect_uri"`
		Scopes      []string `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	parsed, err := url.Parse(body.AuthURL)
	if err != nil {
		t.Fatalf("auth_url is not a URL: %v", err)
	}
	if parsed.Host != "accounts.google.com" {
		t.Errorf("auth host = %q, want the Google consent screen", parsed.Host)
	}
	query := parsed.Query()
	if query.Get("client_id") != "gid" {
		t.Errorf("client_id = %q, want the remote's own OAuth app", query.Get("client_id"))
	}
	if query.Get("redirect_uri") != "https://box.example"+callbackPath {
		t.Errorf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	if query.Get("state") == "" {
		t.Error("no state was issued, so the callback could not be bound to this remote")
	}
	if query.Get("access_type") != "offline" {
		t.Error("Google Drive was not asked for a refresh token")
	}
	if !strings.Contains(query.Get("scope"), "auth/drive") {
		t.Errorf("scope = %q, want Drive access", query.Get("scope"))
	}
}

func TestOAuthStartExplainsAMissingOAuthApp(t *testing.T) {
	fakeRcloneConfigured(t, "gdrive:", `{"gdrive":{"type":"drive"}}`)
	for _, name := range []string{"DRIVE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_ID", "DRIVE_OAUTH_CLIENT_SECRET", "GOOGLE_OAUTH_CLIENT_SECRET"} {
		t.Setenv(name, "")
	}
	req := httptest.NewRequest(http.MethodPost, "https://box.example/api/v1/storage/remotes/gdrive/oauth/start", strings.NewReader("{}"))
	req.SetPathValue("name", "gdrive")
	rec := httptest.NewRecorder()
	(&server{}).handleOAuthStart(rec, req)
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want 424 with an explanation: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "client_id") {
		t.Errorf("the failure does not say what to configure: %s", rec.Body.String())
	}
}

func TestOAuthCallbackStoresTheTokenAndReportsTheProbe(t *testing.T) {
	logPath := fakeRcloneConfigured(t, "gdrive:", gdriveConfigDump)
	state, err := pendingLogins.put(pendingOAuth{remote: "gdrive", backendType: "drive", redirectURI: "https://box.example" + callbackPath, clientID: "gid", clientSecret: "gsecret"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	original := exchangeOAuthCodeFunc
	t.Cleanup(func() { exchangeOAuthCodeFunc = original })
	var sawCode, sawRedirect string
	exchangeOAuthCodeFunc = func(_ context.Context, _ oauthBackend, clientID, clientSecret, code, redirectURI string) (oauthTokenResponse, error) {
		if clientID != "gid" || clientSecret != "gsecret" {
			t.Errorf("the exchange used client %q/%q, want the remote's own app", clientID, clientSecret)
		}
		sawCode, sawRedirect = code, redirectURI
		return oauthTokenResponse{AccessToken: "at", RefreshToken: "rt", TokenType: "Bearer", ExpiresIn: 3600}, nil
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, callbackPath+"?state="+state+"&code=the-code", nil)
	(&server{}).handleOAuthCallback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("the callback answered %q; a browser lands here, not a client", rec.Header().Get("Content-Type"))
	}
	if sawCode != "the-code" || sawRedirect != "https://box.example"+callbackPath {
		t.Errorf("exchange saw code %q / redirect %q", sawCode, sawRedirect)
	}
	if _, ok := pendingLogins.take(state); ok {
		t.Error("the state survived the callback")
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake rclone was never invoked: %v", err)
	}
	got := string(logged)
	// The token has to reach rclone as one compact argv element, and the update
	// has to be non-interactive or rclone starts a flow of its own and the
	// request never answers.
	if !strings.Contains(got, `config update gdrive token {"access_token":"at"`) || !strings.Contains(got, `"refresh_token":"rt"`) {
		t.Errorf("the token rclone was given is not the one the provider returned:\n%s", got)
	}
	if !strings.Contains(got, "--non-interactive") {
		t.Errorf("the token update was not run non-interactively:\n%s", got)
	}
	if !strings.Contains(got, "lsd gdrive:") {
		t.Errorf("the account was never probed after sign-in:\n%s", got)
	}
	if !strings.Contains(rec.Body.String(), "connected") {
		t.Errorf("the browser was not told the account works: %s", rec.Body.String())
	}
}

func TestOAuthCallbackRefusesAnExpiredState(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, callbackPath+"?state=stale&code=c", nil)
	(&server{}).handleOAuthCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("the operator is not told what to do: %s", rec.Body.String())
	}
}
