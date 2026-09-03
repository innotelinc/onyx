package infisical

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRef(t *testing.T) {
	cases := []struct {
		in   string
		name string
		ok   bool
	}{
		{"infisical://S3_SECRET_KEY", "S3_SECRET_KEY", true},
		{"infisical://certs.tenant.1.key", "certs.tenant.1.key", true},
		{"infisical://  spaced  ", "spaced", true},
		{"infisical://", "", false},
		{"plain-secret", "", false},
		{"", "", false},
		{"vault://path#key", "", false},
	}
	for _, c := range cases {
		name, ok := Ref(c.in)
		if name != c.name || ok != c.ok {
			t.Errorf("Ref(%q) = (%q, %v), want (%q, %v)", c.in, name, ok, c.name, c.ok)
		}
	}
}

func TestConfigFromEnvAndEnabled(t *testing.T) {
	t.Setenv(EnvVarAddr, "http://127.0.0.1:8383")
	t.Setenv(EnvVarToken, "tok")
	t.Setenv(EnvVarWorkspaceID, "ws")
	t.Setenv(EnvVarEnvironment, "")

	cfg := ConfigFromEnv()
	if !cfg.Enabled() {
		t.Fatal("expected enabled config")
	}
	if cfg.Environment != "prod" {
		t.Errorf("default environment = %q, want prod", cfg.Environment)
	}

	t.Setenv(EnvVarToken, "")
	if ConfigFromEnv().Enabled() {
		t.Error("expected disabled config without token")
	}
}

func TestResolveEnvPassthrough(t *testing.T) {
	c := New(Config{})
	for _, v := range []string{"plain", "", "vault://x#y"} {
		got, err := c.ResolveEnv(context.Background(), v)
		if err != nil || got != v {
			t.Errorf("ResolveEnv(%q) = (%q, %v), want (%q, nil)", v, got, err, v)
		}
	}
}

func TestResolveEnvUnconfiguredRef(t *testing.T) {
	c := New(Config{})
	if _, err := c.ResolveEnv(context.Background(), "infisical://SECRET"); err == nil {
		t.Fatal("expected error resolving a ref without configured Infisical")
	}
}

func TestReadSecret(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Query().Get("workspaceId") != "ws" || r.URL.Query().Get("environment") != "prod" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"secret": map[string]string{"secretKey": "S3_SECRET_KEY", "secretValue": "hunter2"},
		})
	}))
	defer srv.Close()

	c := New(Config{Addr: srv.URL, Token: "tok", WorkspaceID: "ws", Environment: "prod"})
	got, err := c.ReadSecret(context.Background(), "S3_SECRET_KEY")
	if err != nil {
		t.Fatalf("ReadSecret: %v", err)
	}
	if got != "hunter2" {
		t.Errorf("ReadSecret = %q, want hunter2", got)
	}
	if gotPath != "/api/v3/secrets/raw/S3_SECRET_KEY" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestReadSecretNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"secret not found"}`))
	}))
	defer srv.Close()

	c := New(Config{Addr: srv.URL, Token: "t", WorkspaceID: "w"})
	if _, err := c.ReadSecret(context.Background(), "MISSING"); err == nil {
		t.Fatal("expected error for missing secret")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention HTTP 404: %v", err)
	}
}

func TestResolveEnvRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"secret": map[string]string{"secretValue": "from-infisical"},
		})
	}))
	defer srv.Close()

	c := New(Config{Addr: srv.URL, Token: "t", WorkspaceID: "w"})
	got, err := c.ResolveEnv(context.Background(), "infisical://S3_ACCESS_KEY")
	if err != nil || got != "from-infisical" {
		t.Fatalf("ResolveEnv(ref) = (%q, %v)", got, err)
	}
}

func TestWriteSecret(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v3/secrets/raw/S3_ACCESS_KEY" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(Config{Addr: srv.URL, Token: "t", WorkspaceID: "ws", Environment: "staging"})
	if err := c.WriteSecret(context.Background(), "S3_ACCESS_KEY", "ak"); err != nil {
		t.Fatalf("WriteSecret: %v", err)
	}
	if body["secretValue"] != "ak" || body["workspaceId"] != "ws" ||
		body["environment"] != "staging" || body["type"] != "shared" {
		t.Errorf("unexpected payload: %#v", body)
	}
}

func TestMirror(t *testing.T) {
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/secrets/raw/FAIL" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writes = append(writes, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{Addr: srv.URL, Token: "t", WorkspaceID: "w"})
	written, errs := c.Mirror(context.Background(), map[string]string{
		"S3_ACCESS_KEY": "ak",          // plain → mirrored
		"S3_SECRET_KEY": "infisical://X", // already a ref → skipped
		"EMPTY":         "",             // empty → skipped
		"FAIL":          "boom",         // upstream error → collected
	})
	if len(written) != 1 || written[0] != "S3_ACCESS_KEY" {
		t.Errorf("written = %#v", written)
	}
	if len(errs) != 1 {
		t.Errorf("errs = %#v, want 1", errs)
	}

	// Unconfigured client mirrors nothing, errors nothing.
	if w, e := New(Config{}).Mirror(context.Background(), map[string]string{"A": "b"}); len(w) != 0 || len(e) != 0 {
		t.Errorf("unconfigured mirror = (%#v, %#v)", w, e)
	}
}

func TestStatus(t *testing.T) {
	t.Run("not configured", func(t *testing.T) {
		if s := New(Config{}).Status(context.Background()); s != "not-configured" {
			t.Errorf("status = %q", s)
		}
	})
	t.Run("ok on 404 probe", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		c := New(Config{Addr: srv.URL, Token: "t", WorkspaceID: "w"})
		if s := c.Status(context.Background()); s != "ok" {
			t.Errorf("status = %q, want ok", s)
		}
	})
	t.Run("error on network failure", func(t *testing.T) {
		// Port 1 is never listening; the request fails fast.
		c := New(Config{Addr: "http://127.0.0.1:1", Token: "t", WorkspaceID: "w"})
		if s := c.Status(context.Background()); !strings.HasPrefix(s, "error:") {
			t.Errorf("status = %q, want error:", s)
		}
	})
}