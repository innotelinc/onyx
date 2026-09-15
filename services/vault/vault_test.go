package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in    string
		mount string
		path  string
		key   string
		ok    bool
	}{
		{"vault://cerulean/onyx#S3_ACCESS_KEY", "cerulean", "onyx", "S3_ACCESS_KEY", true},
		{"vault://cerulean/onyx/api#CERULEAN_API_TOKEN", "cerulean", "onyx/api", "CERULEAN_API_TOKEN", true},
		{"vault://cerulean/onyx", "cerulean", "onyx", "", true},
		{"vault://onyx#KEY", "onyx", "", "KEY", true},
		{"vault:///cerulean/onyx#KEY", "cerulean", "onyx", "KEY", true},
		{"vault://  cerulean/onyx#spaced  ", "cerulean", "onyx", "spaced", true},
		{"vault://", "", "", "", false},
		{"vault:///#KEY", "", "", "", false},
		{"infisical://S3_ACCESS_KEY", "", "", "", false},
		{"plain-secret", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, c := range cases {
		ref, ok := ParseRef(c.in)
		if ok != c.ok || ref.Mount != c.mount || ref.Path != c.path || ref.Key != c.key {
			t.Errorf("ParseRef(%q) = (%+v, %v), want mount=%q path=%q key=%q ok=%v",
				c.in, ref, ok, c.mount, c.path, c.key, c.ok)
		}
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv(EnvVarAddr, "http://vault.example:8200/")
	t.Setenv(EnvVarToken, "tok")
	t.Setenv(EnvVarTokenFile, "/nonexistent")
	t.Setenv(EnvVarPrefix, "")
	t.Setenv(EnvVarPath, "")
	t.Setenv(EnvVarSkipVerify, "1")

	cfg := ConfigFromEnv()
	if cfg.Addr != "http://vault.example:8200" {
		t.Errorf("Addr = %q, want the trailing slash trimmed", cfg.Addr)
	}
	if cfg.Prefix != DefaultPrefix || cfg.Path != DefaultPath {
		t.Errorf("defaults = %q/%q, want %q/%q", cfg.Prefix, cfg.Path, DefaultPrefix, DefaultPath)
	}
	if !cfg.SkipVerify {
		t.Error("expected SkipVerify for VAULT_SKIP_VERIFY=1")
	}
	if !cfg.Enabled() {
		t.Error("expected enabled config with an address and a token")
	}

	// Both halves of the credential must go: a configured token file alone is
	// still a working store, because the token is only read when it is needed.
	t.Setenv(EnvVarToken, "")
	t.Setenv(EnvVarTokenFile, "")
	if ConfigFromEnv().Enabled() {
		t.Error("expected disabled config with no token and no token file")
	}

	// Either one on its own is enough to count as configured.
	t.Setenv(EnvVarToken, "tok")
	if !ConfigFromEnv().Enabled() {
		t.Error("expected enabled config with a token and no token file")
	}
	t.Setenv(EnvVarToken, "")
	t.Setenv(EnvVarTokenFile, "/nonexistent")
	if !ConfigFromEnv().Enabled() {
		t.Error("expected enabled config with a token file and no token")
	}
}

func TestConfigFromEnvTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "onyx.token")
	if err := os.WriteFile(tokenPath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVarAddr, "http://127.0.0.1:8200")
	t.Setenv(EnvVarToken, "")
	t.Setenv(EnvVarTokenFile, tokenPath)

	c := New(ConfigFromEnv())
	got, err := c.token()
	if err != nil {
		t.Fatalf("token() from file: %v", err)
	}
	if got != "file-token" {
		t.Errorf("token() = %q, want the file contents with the newline trimmed", got)
	}
}

func TestResolveEnvPassthrough(t *testing.T) {
	c := New(Config{})
	for _, v := range []string{"plain", "", "infisical://S3_ACCESS_KEY"} {
		got, err := c.ResolveEnv(context.Background(), v, nil)
		if err != nil || got != v {
			t.Errorf("ResolveEnv(%q) = (%q, %v), want (%q, nil)", v, got, err, v)
		}
	}
}

func TestResolveEnvRequiresConfiguration(t *testing.T) {
	c := New(Config{}) // no addr, no token
	_, err := c.ResolveEnv(context.Background(), "vault://cerulean/onyx#KEY", nil)
	if err == nil {
		t.Fatal("expected an error resolving a reference with no configured store")
	}
	if !strings.Contains(err.Error(), EnvVarAddr) {
		t.Errorf("error should name %s: %v", EnvVarAddr, err)
	}
}

// kv2Server answers KV v2 reads for <prefix>/data/<path> out of a fixture map.
func kv2Server(t *testing.T, prefix string, fixture map[string]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Vault-Token"); got != "test-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		base := "/v1/" + prefix + "/data/"
		if !strings.HasPrefix(r.URL.Path, base) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		secret, ok := fixture[strings.TrimPrefix(r.URL.Path, base)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"data": secret, "metadata": map[string]any{"version": 1}},
		})
	}))
}

func TestResolveEnvReadsKV2(t *testing.T) {
	srv := kv2Server(t, DefaultPrefix, map[string]map[string]any{
		"onyx":         {"S3_ACCESS_KEY": "from-vault", "S3_SECRET_KEY": "s3cr3t"},
		"onyx/api":     {"CERULEAN_API_TOKEN": "api-token"},
		"onyx/scratch": {},
	})
	defer srv.Close()

	c := New(Config{Addr: srv.URL, Token: "test-token", Prefix: DefaultPrefix, Path: DefaultPath})

	cases := []struct {
		in   string
		want string
	}{
		{"vault://cerulean/onyx#S3_ACCESS_KEY", "from-vault"},
		{"vault://cerulean/onyx/api#CERULEAN_API_TOKEN", "api-token"},
	}
	for _, c2 := range cases {
		got, err := c.ResolveEnv(context.Background(), c2.in, nil)
		if err != nil {
			t.Fatalf("ResolveEnv(%q): %v", c2.in, err)
		}
		if got != c2.want {
			t.Errorf("ResolveEnv(%q) = %q, want %q", c2.in, got, c2.want)
		}
	}

	// A key the secret does not hold, and a secret that is not there, are both
	// hard errors — never an empty credential.
	if _, err := c.ResolveEnv(context.Background(), "vault://cerulean/onyx#MISSING", nil); err == nil {
		t.Error("expected an error for a key the secret does not hold")
	}
	if _, err := c.ResolveEnv(context.Background(), "vault://cerulean/nope#KEY", nil); err == nil {
		t.Error("expected an error for a secret that does not exist")
	}
	if _, err := c.ResolveEnv(context.Background(), "vault://cerulean/onyx/scratch#KEY", nil); err == nil {
		t.Error("expected an error for an empty secret")
	}
}

func TestReadSecretRejectsNonKV2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"value":"not-nested"}}`)) // KV v1 shape
	}))
	defer srv.Close()

	c := New(Config{Addr: srv.URL, Token: "test-token", Prefix: DefaultPrefix, Path: DefaultPath})
	_, err := c.ReadSecret(context.Background(), DefaultPrefix, DefaultPath)
	if err == nil || !strings.Contains(err.Error(), "KV v2") {
		t.Fatalf("expected a KV v2 explanation, got %v", err)
	}
}

func TestStatus(t *testing.T) {
	// Not configured.
	if got := New(Config{}).Status(context.Background()); got != "not-configured" {
		t.Errorf("Status() with no config = %q, want not-configured", got)
	}

	// A 404 on the stack's own path proves auth + connectivity: reported ok.
	srv := kv2Server(t, DefaultPrefix, map[string]map[string]any{})
	defer srv.Close()
	c := New(Config{Addr: srv.URL, Token: "test-token", Prefix: DefaultPrefix, Path: DefaultPath})
	if got := c.Status(context.Background()); got != "ok" {
		t.Errorf("Status() with a missing secret = %q, want ok", got)
	}

	// A bad token is a real failure, and the message has to say so.
	bad := New(Config{Addr: srv.URL, Token: "wrong", Prefix: DefaultPrefix, Path: DefaultPath})
	if got := bad.Status(context.Background()); !strings.HasPrefix(got, "error:") {
		t.Errorf("Status() with a rejected token = %q, want an error", got)
	}
}

// fakeLegacy stands in for *infisical.Client in the dispatcher test.
type fakeLegacy struct {
	values map[string]string
	calls  []string
}

func (f *fakeLegacy) ResolveEnv(_ context.Context, value string) (string, error) {
	f.calls = append(f.calls, value)
	name := strings.TrimPrefix(value, "infisical://")
	return f.values[name], nil
}

func TestResolveEnvDispatchesToLegacyStore(t *testing.T) {
	c := New(Config{Addr: "http://127.0.0.1:8200", Token: "t", Prefix: DefaultPrefix, Path: DefaultPath})
	legacy := &fakeLegacy{values: map[string]string{"S3_ACCESS_KEY": "from-infisical"}}

	got, err := c.ResolveEnv(context.Background(), "infisical://S3_ACCESS_KEY", legacy)
	if err != nil {
		t.Fatalf("ResolveEnv legacy: %v", err)
	}
	if got != "from-infisical" {
		t.Errorf("legacy resolve = %q, want from-infisical", got)
	}
	if len(legacy.calls) != 1 {
		t.Errorf("legacy store called %d time(s), want 1", len(legacy.calls))
	}

	// A plain value still passes through untouched, and does not touch the store.
	before := len(legacy.calls)
	if got, err := c.ResolveEnv(context.Background(), "plain", legacy); err != nil || got != "plain" {
		t.Errorf("ResolveEnv(plain) = (%q, %v), want (plain, nil)", got, err)
	}
	if len(legacy.calls) != before {
		t.Error("a plain value should not reach the legacy store")
	}
}
