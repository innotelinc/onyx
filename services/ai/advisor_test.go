package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// The advisor's contract is that the local summary is always available and the
// model plane can only improve on it. These tests pin both halves: the config
// resolution order, and the fallback that keeps a broken gateway from turning a
// findings response into an error.

func clearModelEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OMNIROUTE_BASE_URL", "OMNIROUTE_API_KEY", "OMNIROUTE_MODEL",
		"AI_PROVIDER", "AI_API_KEY", "AI_MODEL", "VAULT_ADDR", "INFISICAL_ADDR",
	} {
		t.Setenv(key, "")
	}
}

func TestReadModelConfigPrefersTheGateway(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("OMNIROUTE_BASE_URL", "http://10.10.2.1:20128/v1/")
	t.Setenv("OMNIROUTE_API_KEY", "gateway-key")
	t.Setenv("OMNIROUTE_MODEL", "auto/coding")
	// Both planes set is a config mistake; the shared one wins.
	t.Setenv("AI_PROVIDER", "http://127.0.0.1:11434/v1")
	t.Setenv("AI_API_KEY", "byo-key")

	cfg := readModelConfig()

	if cfg.source != "omniroute" {
		t.Errorf("source = %q, want omniroute", cfg.source)
	}
	if cfg.baseURL != "http://10.10.2.1:20128/v1" {
		t.Errorf("baseURL = %q — the trailing slash should be trimmed", cfg.baseURL)
	}
	if cfg.apiKey != "gateway-key" || cfg.model != "auto/coding" {
		t.Errorf("key/model = %q/%q, want gateway-key/auto/coding", cfg.apiKey, cfg.model)
	}
}

func TestReadModelConfigFallsBackToBYOKey(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("AI_PROVIDER", "http://127.0.0.1:11434/v1")

	cfg := readModelConfig()

	if cfg.source != "byo" || !cfg.configured() {
		t.Fatalf("source = %q configured = %v, want byo/true", cfg.source, cfg.configured())
	}
	if cfg.model != "auto" {
		t.Errorf("model = %q, want the auto default", cfg.model)
	}
	if cfg.apiKey != "" {
		t.Errorf("apiKey = %q, want empty when AI_API_KEY is unset", cfg.apiKey)
	}
}

func TestReadModelConfigUnconfigured(t *testing.T) {
	clearModelEnv(t)

	if cfg := readModelConfig(); cfg.configured() {
		t.Fatalf("configured = true with no model plane set (baseURL %q)", cfg.baseURL)
	}
}

func TestAdviseSendsFindingsAndReturnsText(t *testing.T) {
	clearModelEnv(t)

	var gotPath, gotAuth string
	var gotBody chatRequest

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Pool onyx-pool1 is nearly full; add capacity."}}]}`))
	}))
	defer upstream.Close()

	cfg := modelConfig{baseURL: upstream.URL, apiKey: "gateway-key", model: "auto", source: "omniroute"}
	advice, err := cfg.advise(context.Background(), "storage", []*onyxv1.StorageFinding{
		{Severity: "critical", Code: "low_free_space", Summary: "Pool critically low"},
	})
	if err != nil {
		t.Fatalf("advise: %v", err)
	}

	if advice != "Pool onyx-pool1 is nearly full; add capacity." {
		t.Errorf("advice = %q", advice)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer gateway-key" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBody.Stream {
		t.Error("stream = true, want a single non-streaming completion")
	}
	if gotBody.Model != "auto" {
		t.Errorf("model = %q, want auto", gotBody.Model)
	}
	if len(gotBody.Messages) != 2 {
		t.Fatalf("messages = %d, want a system and a user message", len(gotBody.Messages))
	}
	if !strings.Contains(gotBody.Messages[1].Content, "low_free_space") {
		t.Errorf("user message does not carry the finding: %q", gotBody.Messages[1].Content)
	}
}

func TestAdviseErrorsAreSafeToLog(t *testing.T) {
	clearModelEnv(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key sk-do-not-echo"}`))
	}))
	defer upstream.Close()

	cfg := modelConfig{baseURL: upstream.URL, apiKey: "sk-super-secret", model: "auto", source: "omniroute"}
	_, err := cfg.advise(context.Background(), "storage", []*onyxv1.StorageFinding{{Summary: "x"}})
	if err == nil {
		t.Fatal("advise succeeded against a 401")
	}
	if strings.Contains(err.Error(), "sk-super-secret") {
		t.Errorf("error leaks the configured key: %v", err)
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error should point at the credential: %v", err)
	}
}

func TestNarrateFallsBackToTheLocalSummary(t *testing.T) {
	clearModelEnv(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	findings := []*onyxv1.StorageFinding{{Severity: "warning", Summary: "Pool running low on free space"}}

	local := &server{model: modelConfig{}}
	if got, want := local.narrate(context.Background(), "storage", findings), local.narrative("storage", findings); got != want {
		t.Errorf("unconfigured narrate = %q, want the local summary %q", got, want)
	}

	broken := &server{model: modelConfig{baseURL: upstream.URL, model: "auto", source: "omniroute"}}
	if got, want := broken.narrate(context.Background(), "storage", findings), broken.narrative("storage", findings); got != want {
		t.Errorf("unreachable gateway narrate = %q, want the local summary %q", got, want)
	}
}

func TestNarrateUsesTheModelWhenItAnswers(t *testing.T) {
	clearModelEnv(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The Responses shape, which the gateway can be configured for.
		_, _ = w.Write([]byte(`{"output_text":"Advice from the model."}`))
	}))
	defer upstream.Close()

	s := &server{model: modelConfig{baseURL: upstream.URL, model: "auto", source: "omniroute"}}
	got := s.narrate(context.Background(), "storage", []*onyxv1.StorageFinding{{Summary: "Pool critically low"}})
	if got != "Advice from the model." {
		t.Errorf("narrate = %q, want the model's advice", got)
	}
}

func TestNarrateFallsBackOnAnEmptyAnswer(t *testing.T) {
	clearModelEnv(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"   "}}]}`))
	}))
	defer upstream.Close()

	s := &server{model: modelConfig{baseURL: upstream.URL, model: "auto", source: "omniroute"}}
	findings := []*onyxv1.StorageFinding{{Summary: "Pool critically low"}}

	if got, want := s.narrate(context.Background(), "storage", findings), s.narrative("storage", findings); got != want {
		t.Errorf("empty answer narrate = %q, want the local summary %q", got, want)
	}
}

func TestResolveSecretEnvIsANoOpWithoutASecretStore(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("OMNIROUTE_API_KEY", "plain-value")

	if err := resolveSecretEnv(context.Background(), "OMNIROUTE_API_KEY"); err != nil {
		t.Fatalf("resolveSecretEnv: %v", err)
	}
	if got := readModelConfig().apiKey; got != "" {
		t.Errorf("apiKey = %q — readModelConfig should not invent one", got)
	}
}

// A reference with no secret store configured must fail at boot rather than
// hand a literal `vault://…` string to the gateway as a bearer token.
func TestResolveSecretEnvFailsOnAnUnresolvableReference(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("OMNIROUTE_API_KEY", "vault://cerulean/onyx#OMNIROUTE_API_KEY")

	err := resolveSecretEnv(context.Background(), "OMNIROUTE_API_KEY")
	if err == nil {
		t.Fatal("resolveSecretEnv accepted a reference with no Vault configured")
	}
	if !strings.Contains(err.Error(), "OMNIROUTE_API_KEY") {
		t.Errorf("error should name the key: %v", err)
	}
}
