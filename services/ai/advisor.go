package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"onyx.dev/onyx/services/infisical"
	"onyx.dev/onyx/services/vault"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// The AI plane, as ONYX consumes it (docs/design/11 §6.5).
//
// ONYX does not own inference. Cerulean's platform runs ONE OmniRoute
// (Group 2, mesh 10.10.2.1:20128) and every platform points at it, so
// `onyx-ai` gets an `OMNIROUTE_BASE_URL` and a **key path** rather than an
// upstream provider key: the provider accounts rotate in the gateway's
// dashboard, and ONYX never ships one.
//
// Local-first is preserved rather than replaced:
//
//   - no gateway and no provider configured → the deterministic heuristics and
//     the local sentence remain the whole answer, and nothing leaves the box;
//   - the gateway is configured → findings are sent to it for natural-language
//     advice, and if it is unreachable, slow, or answers with nonsense the local
//     sentence is what the caller gets. A model is an improvement to the answer,
//     never a dependency of it.
//
// `AI_PROVIDER`/`AI_API_KEY`/`AI_MODEL` stay supported as the documented
// BYO-key escape hatch (docs/design/07 §Privacy); the gateway wins when both
// are set, because two configured model planes is a config mistake and the
// shared one is the platform's answer.

const (
	// A finding list is small, so a request that has not answered in 20s is not
	// going to: waiting longer would hold the RPC open for nothing.
	advisorTimeout = 20 * time.Second

	// Conservative: an advisor summary is a sentence or two, and a model asked
	// for more than that is inventing detail the findings do not contain.
	advisorMaxTokens = 400

	advisorSystemPrompt = "You are the Innotel Platform Stack storage advisor. " +
		"Rewrite the supplied findings as plain-language advice for an operator. " +
		"Use only the supplied findings: do not invent capacities, timings or " +
		"commands. Lead with the most severe finding, keep it under 120 words, " +
		"and do not add a preamble or a sign-off."
)

// modelConfig is the configured AI plane, or the absence of one.
type modelConfig struct {
	baseURL string
	apiKey  string
	model   string
	// source is "omniroute", "byo", or "" when nothing is configured. It is
	// reported in logs and never carries the key.
	source string
}

// configured reports whether there is a model plane to call at all.
func (c modelConfig) configured() bool { return c.baseURL != "" }

// readModelConfig reads the gateway pair first, then the BYO-key hook.
//
// Environment only — the caller has already resolved any `vault://` reference
// (see resolveSecretEnv), so nothing here reaches a secret store, and nothing
// here logs a value.
func readModelConfig() modelConfig {
	if baseURL := strings.TrimSpace(os.Getenv("OMNIROUTE_BASE_URL")); baseURL != "" {
		model := strings.TrimSpace(os.Getenv("OMNIROUTE_MODEL"))
		if model == "" {
			model = "auto"
		}
		return modelConfig{
			baseURL: strings.TrimRight(baseURL, "/"),
			apiKey:  strings.TrimSpace(os.Getenv("OMNIROUTE_API_KEY")),
			model:   model,
			source:  "omniroute",
		}
	}

	if provider := strings.TrimSpace(os.Getenv("AI_PROVIDER")); provider != "" {
		model := strings.TrimSpace(os.Getenv("AI_MODEL"))
		if model == "" {
			model = "auto"
		}
		return modelConfig{
			baseURL: strings.TrimRight(provider, "/"),
			apiKey:  strings.TrimSpace(os.Getenv("AI_API_KEY")),
			model:   model,
			source:  "byo",
		}
	}

	return modelConfig{}
}

// resolveSecretEnv resolves `vault://` references in env values before a
// credential is read, once, at startup — the pattern onyx-objectstore uses for
// its S3 credentials (and the reason ONYX can run `.env` with references).
//
// Only the two credential-shaped keys are resolved: a plain value passes
// through untouched, and a reference that cannot be resolved is a **hard
// failure**, never an empty credential. It fails here, at boot, where the
// message names the secret store, instead of later as a 401 from a gateway that
// cannot say which key it rejected.
func resolveSecretEnv(ctx context.Context, keys ...string) error {
	vcfg := vault.ConfigFromEnv()
	icfg := infisical.ConfigFromEnv()
	if !vcfg.Enabled() && !icfg.Enabled() {
		// No secret store on this box. A plain value is the normal local case
		// and passes through; a reference is not — without this check it would
		// be sent to the gateway as `Bearer vault://…`, which is the silent
		// wrong-credential failure the whole convention exists to prevent.
		for _, key := range keys {
			if _, isRef := vault.ParseRef(os.Getenv(key)); isRef {
				return fmt.Errorf(
					"resolve %s: the value is a vault:// reference but no secret store is configured "+
						"(set VAULT_ADDR and VAULT_TOKEN/VAULT_TOKEN_FILE, or put the plain value in .env)",
					key,
				)
			}
		}
		return nil
	}

	var legacy vault.LegacyResolver
	if icfg.Enabled() {
		legacy = infisical.New(icfg)
	}
	client := vault.New(vcfg)

	for _, key := range keys {
		value := os.Getenv(key)
		resolved, err := client.ResolveEnv(ctx, value, legacy)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", key, err)
		}
		if resolved != value {
			if err := os.Setenv(key, resolved); err != nil {
				return fmt.Errorf("set %s: %w", key, err)
			}
			// Key name only — never the value.
			slog.Info("resolved secret reference", "key", key)
		}
	}
	return nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	// Some gateways answer the Responses shape instead of Chat Completions.
	OutputText string `json:"output_text"`
}

// advise asks the configured model plane to rewrite findings as advice.
//
// The findings are the only thing that leaves the box, and only here. An error
// is always returned with a message fit for a log line: it never contains the
// key, because the key is in a request header the error paths do not echo.
func (c modelConfig) advise(ctx context.Context, kind string, findings []*onyxv1.StorageFinding) (string, error) {
	if !c.configured() {
		return "", errors.New("no model plane configured")
	}
	if len(findings) == 0 {
		return "", errors.New("no findings to advise on")
	}

	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: advisorSystemPrompt},
			{Role: "user", Content: findingsPrompt(kind, findings)},
		},
		Stream:      false,
		Temperature: 0.2,
		MaxTokens:   advisorMaxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, advisorTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	response, err := http.DefaultClient.Do(req)
	if err != nil {
		// The error text can carry the URL; it never carries the header.
		return "", fmt.Errorf("call the model plane: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 400))
		hint := ""
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			hint = " (the configured gateway key was rejected)"
		}
		return "", fmt.Errorf("model plane answered %d%s: %s", response.StatusCode, hint, strings.TrimSpace(string(detail)))
	}

	var parsed chatResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	if len(parsed.Choices) > 0 {
		if text := strings.TrimSpace(parsed.Choices[0].Message.Content); text != "" {
			return text, nil
		}
	}
	if text := strings.TrimSpace(parsed.OutputText); text != "" {
		return text, nil
	}
	return "", errors.New("the model plane returned no text")
}

// findingsPrompt renders findings in the shape the model is asked to rewrite.
// Deliberately the findings only — no hostnames, no paths, no pool metadata
// beyond what a finding already quotes back to the operator.
func findingsPrompt(kind string, findings []*onyxv1.StorageFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Kind: %s\nFindings (%d):\n", kind, len(findings))
	for _, f := range findings {
		fmt.Fprintf(&b, "- severity=%s code=%s summary=%q detail=%q action=%q\n",
			f.GetSeverity(), f.GetCode(), f.GetSummary(), f.GetDetail(), f.GetAction())
	}
	return b.String()
}
