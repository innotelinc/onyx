package main

import (
	"net/http"
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

// A connection is only usable if the secret reaches rclone exactly as the
// operator typed it: obscuring an S3 secret_access_key made every request fail
// with SignatureDoesNotMatch, so the argv is pinned here. A fake rclone on PATH
// records what the handler actually runs — no rclone binary or network needed.
func TestCreateRemotePassesSecretsThroughRaw(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rclone.args")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := &server{}
	body := `{"name":"onyx-test","type":"s3","params":{"provider":"Other","access_key_id":"AKIAEXAMPLE","secret_access_key":"hunter2","endpoint":"http://minio:9000"}}`
	rec := post(t, s.handleCreateRemote, "/api/v1/storage/remotes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("fake rclone was never invoked: %v", err)
	}
	got := string(logged)
	if !strings.Contains(got, "config create onyx-test s3") {
		t.Errorf("the remote was not created through rclone:\n%s", got)
	}
	if !strings.Contains(got, "secret_access_key hunter2") {
		t.Errorf("the secret was not passed through raw:\n%s", got)
	}
	if strings.Contains(got, "obscure") {
		t.Errorf("the handler obscured a secret itself:\n%s", got)
	}
}
