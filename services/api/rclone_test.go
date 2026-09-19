package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cloud-storage form builds an rclone command line from operator input, so
// these tests pin the pieces that keep that argv a closed set.

func TestValidateRemoteName(t *testing.T) {
	for _, name := range []string{"my-cloud", "onyx_backups", "b2.prod", "a", "AWS2026"} {
		if err := validateRemoteName(name); err != nil {
			t.Errorf("validateRemoteName(%q) = %v, want nil", name, err)
		}
	}
	// A name becomes an argv element: ':' and '/' are rclone separators, a
	// leading '-' would be read as a flag, and whitespace or newlines must
	// never reach the command line.
	for _, name := range []string{"", "-flag", "a:b", "a/b", "a b", "a\nb", "a$b", strings.Repeat("x", 65)} {
		if err := validateRemoteName(name); err == nil {
			t.Errorf("validateRemoteName(%q) = nil, want an error", name)
		}
	}
}

func TestProviderCatalogIsSelfConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range rcloneProviders() {
		if p.Type == "" || p.Label == "" {
			t.Errorf("provider %+v is missing a type or label", p)
		}
		if seen[p.Type] {
			t.Errorf("provider %q is listed twice", p.Type)
		}
		seen[p.Type] = true
		fields := map[string]bool{}
		for _, f := range p.Fields {
			fields[f] = true
		}
		for _, required := range p.Required {
			if !fields[required] {
				t.Errorf("provider %q requires %q, which is not one of its fields", p.Type, required)
			}
		}
		if p.OAuth && len(p.Required) > 0 {
			t.Errorf("provider %q is OAuth but demands stored credentials", p.Type)
		}
	}
}

func TestProviderByType(t *testing.T) {
	if p, ok := providerByType("S3"); !ok || p.Type != "s3" {
		t.Fatalf("providerByType(\"S3\") = %+v, %v; want the s3 provider", p, ok)
	}
	if _, ok := providerByType("  b2  "); !ok {
		t.Error("providerByType should trim whitespace")
	}
	if _, ok := providerByType("nfs-share"); ok {
		t.Error("unknown backends must be rejected rather than guessed")
	}
}

func TestLastLinesKeepsTheSummary(t *testing.T) {
	output := "a\nb\nc\nd\ne\nf\n"
	if got := lastLines(output, 2); got != "e\nf" {
		t.Errorf("lastLines = %q, want %q", got, "e\nf")
	}
	if got := lastLines("only", 5); got != "only" {
		t.Errorf("lastLines = %q, want %q", got, "only")
	}
}

func TestContainsString(t *testing.T) {
	if !containsString([]string{"a", "b"}, "b") {
		t.Error("containsString missed an existing entry")
	}
	if containsString([]string{"a"}, "b") || containsString(nil, "b") {
		t.Error("containsString found a missing entry")
	}
}

// fakeRclone puts a stub rclone on PATH that records the argv of every
// invocation, and returns the path of that log. What the handler *runs* is the
// contract under test here, so no rclone install or network is involved.
func fakeRclone(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rclone.args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// fakeRcloneWith is fakeRclone plus an answer for `listremotes`, which the
// token handler consults before it will write anything. The script logs every
// argv it is invoked with, so what the handler runs stays the contract under
// test — no rclone install and no network.
func fakeRcloneWith(t *testing.T, listremotes string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rclone.args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
		"if [ \"$1\" = listremotes ]; then printf '%s\\n' '" + listremotes + "'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// The token step is the only way an OAuth backend (Google Drive, OneDrive, …)
// can be connected from the console, so what it accepts has to be exactly what
// `rclone authorize` prints — markers, pretty-printed JSON and all — and what it
// hands rclone has to be one compact argv element with the markers stripped.
func TestExtractRcloneToken(t *testing.T) {
	pasted := "Paste the following into your remote machine --->\n" +
		"{\n\t\"access_token\": \"ya29.example\",\n\t\"refresh_token\": \"1//example\"\n}\n" +
		"<---End paste"
	got, err := extractRcloneToken(pasted)
	if err != nil {
		t.Fatalf("extractRcloneToken(whole authorize output) = %v", err)
	}
	if got != `{"access_token":"ya29.example","refresh_token":"1//example"}` {
		t.Errorf("extracted token = %s, want the compacted JSON alone", got)
	}
	// A refresh-only token is what some providers hand back.
	if _, err := extractRcloneToken(`{"refresh_token":"1//x"}`); err != nil {
		t.Errorf("a refresh-only token should be accepted: %v", err)
	}
	// Pasting something that is not a token must fail with an explanation, not
	// be written to the config as a broken secret.
	for _, bad := range []string{"", "   ", "no json here", `{"access_token":`, `{"foo":"bar"}`, `[]`} {
		if got, err := extractRcloneToken(bad); err == nil {
			t.Errorf("extractRcloneToken(%q) = %q, want an error", bad, got)
		}
	}
}

func TestSetRemoteTokenStoresWhatRclonePrinted(t *testing.T) {
	logPath := fakeRcloneWith(t, "gdrive:")
	pasted := "Paste the following into your remote machine --->\n" +
		"{\n\"access_token\": \"ya29.example\",\n\"token_type\": \"Bearer\",\n\"refresh_token\": \"1//example\",\n\"expiry\": \"2030-01-01T00:00:00Z\"\n}\n" +
		"<---End paste\n"
	body, err := json.Marshal(remoteTokenBody{Token: pasted, ClientID: "client-id", ClientSecret: "client-secret"})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/storage/remotes/gdrive/token", bytes.NewReader(body))
	req.SetPathValue("name", "gdrive")
	(&server{}).handleSetRemoteToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result["stored"] != true {
		t.Errorf("response did not confirm the token was stored: %s", rec.Body.String())
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake rclone was never invoked: %v", err)
	}
	got := string(logged)
	if !strings.Contains(got, `config update gdrive token {"access_token":"ya29.example"`) {
		t.Errorf("the token did not reach rclone as one compact argv element:\n%s", got)
	}
	if strings.Contains(got, "Paste the following") || strings.Contains(got, "End paste") {
		t.Errorf("rclone's paste markers leaked into the config:\n%s", got)
	}
	for _, want := range []string{"client_id client-id", "client_secret client-secret"} {
		if !strings.Contains(got, want) {
			t.Errorf("the OAuth app credentials were not passed through (%q):\n%s", want, got)
		}
	}
	if !strings.Contains(got, "lsd gdrive:") {
		t.Errorf("the token was stored without probing the account:\n%s", got)
	}
	// Without --non-interactive rclone starts its own authorization flow when it
	// cannot validate the token, and the request hangs instead of answering.
	if !strings.Contains(got, "config update gdrive token") || !strings.Contains(got, "--non-interactive") {
		t.Errorf("the token update must run non-interactively:\n%s", got)
	}
}

func TestSetRemoteTokenRejectsBadInput(t *testing.T) {
	fakeRcloneWith(t, "gdrive:")
	call := func(t *testing.T, name, body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/storage/remotes/"+name+"/token", strings.NewReader(body))
		req.SetPathValue("name", name)
		(&server{}).handleSetRemoteToken(rec, req)
		return rec.Code
	}
	if code := call(t, "gdrive", `{"token":"not a token"}`); code != http.StatusBadRequest {
		t.Errorf("a non-token body = %d, want 400", code)
	}
	if code := call(t, "gdrive", `{"token":"{\"foo\":1}"}`); code != http.StatusBadRequest {
		t.Errorf("a token with no access/refresh token = %d, want 400", code)
	}
	// A remote that was never saved cannot be connected: the operator is told to
	// save the target first rather than getting a config entry out of nowhere.
	if code := call(t, "missing", `{"token":"{\"access_token\":\"x\"}"}`); code != http.StatusNotFound {
		t.Errorf("an unknown remote = %d, want 404", code)
	}
	if code := call(t, "bad:name", `{"token":"{\"access_token\":\"x\"}"}`); code != http.StatusBadRequest {
		t.Errorf("a remote name rclone cannot take = %d, want 400", code)
	}
}

// The value the operator types has to reach rclone exactly as typed. rclone
// obscures the options a backend marks as passwords (`pass`) and stores the rest
// verbatim (an S3 `secret_access_key`, a B2 `key`), so obscuring here corrupted
// the raw fields — an S3 remote then failed every request with
// SignatureDoesNotMatch — and double-handed a password. Every provider the form
// offers is pinned, so a pre-obscuring step cannot come back.
func TestCreateRemotePassesSecretsThroughRaw(t *testing.T) {
	cases := []struct {
		provider string
		params   map[string]string
	}{
		{"s3", map[string]string{"provider": "Other", "access_key_id": "AKIAEXAMPLE", "secret_access_key": "s3-secret", "endpoint": "http://minio:9000"}},
		{"b2", map[string]string{"account": "b2-account", "key": "b2-secret"}},
		{"azureblob", map[string]string{"account": "azure-account", "key": "azure-secret"}},
		{"sftp", map[string]string{"host": "sftp.example.com", "user": "operator", "pass": "sftp-secret"}},
		{"smb", map[string]string{"host": "smb.example.com", "user": "operator", "pass": "smb-secret"}},
		{"webdav", map[string]string{"url": "https://dav.example.com", "user": "operator", "pass": "webdav-secret"}},
		{"ftp", map[string]string{"host": "ftp.example.com", "user": "operator", "pass": "ftp-secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			logPath := fakeRclone(t)
			name := "remote-" + tc.provider
			body, err := json.Marshal(createRemoteBody{Name: name, Type: tc.provider, Params: tc.params})
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			rec := post(t, (&server{}).handleCreateRemote, "/api/v1/storage/remotes", string(body))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
			}
			logged, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("fake rclone was never invoked: %v", err)
			}
			got := string(logged)
			if !strings.Contains(got, "config create "+name+" "+tc.provider) {
				t.Errorf("the remote was not created through rclone:\n%s", got)
			}
			for field, value := range tc.params {
				if !strings.Contains(got, field+" "+value) {
					t.Errorf("the %s value was not passed through raw:\n%s", field, got)
				}
			}
			if strings.Contains(got, "obscure") {
				t.Errorf("the handler obscured a secret itself:\n%s", got)
			}
		})
	}
}
