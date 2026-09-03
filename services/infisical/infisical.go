// Package infisical is the ONYX client for Infisical (SecretOps), the Innotel
// Platform Stack's single source of truth for secrets (docs/design/11 §6.9).
//
// It mirrors the Cerulean integration: .env values may be either plain text
// or `infisical://<name>` references. A reference is resolved at startup
// against the Infisical API, so real credentials never live in .env (which is
// git-ignored anyway) or in the compose file. Plain values pass through
// unchanged, and Mirror() can push plain values into Infisical to seed the
// switch to references.
//
// Environment contract (written back to .env by scripts/infisical-setup.py):
//
//	INFISICAL_ADDR           base URL, default http://localhost:8383
//	INFISICAL_TOKEN          scoped service token
//	INFISICAL_WORKSPACE_ID   workspace (project) the token is scoped to
//	INFISICAL_ENVIRONMENT    environment folder, default "prod"
package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// EnvVarAddr etc. are the env keys this package reads. Keep them in one
	// place so every service wires the same contract.
	EnvVarAddr        = "INFISICAL_ADDR"
	EnvVarToken       = "INFISICAL_TOKEN"
	EnvVarWorkspaceID = "INFISICAL_WORKSPACE_ID"
	EnvVarEnvironment = "INFISICAL_ENVIRONMENT"
)

// Config is the runtime secret-store configuration, sourced from the
// environment. Services construct it once at startup.
type Config struct {
	Addr        string // base URL, no trailing slash
	Token       string // scoped service token
	WorkspaceID string
	Environment string
}

// ConfigFromEnv builds Config from the INFISICAL_* environment contract.
func ConfigFromEnv() Config {
	cfg := Config{
		Addr:        strings.TrimRight(os.Getenv(EnvVarAddr), "/"),
		Token:       os.Getenv(EnvVarToken),
		WorkspaceID: os.Getenv(EnvVarWorkspaceID),
		Environment: os.Getenv(EnvVarEnvironment),
	}
	if cfg.Environment == "" {
		cfg.Environment = "prod"
	}
	return cfg
}

// Enabled reports whether a working Infisical client is configured.
func (c Config) Enabled() bool {
	return c.Addr != "" && c.Token != "" && c.WorkspaceID != ""
}

// Ref parses an `infisical://<name>` reference. Returns ok=false for any
// value that is not a reference (including empty names).
func Ref(value string) (name string, ok bool) {
	const prefix = "infisical://"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	name = strings.TrimSpace(value[len(prefix):])
	return name, name != ""
}

// Client talks to the Infisical API. The zero value is never used directly —
// construct with New.
type Client struct {
	cfg Config
	hc  *http.Client
}

// New returns a client for cfg. The HTTP client carries a 15 s per-request
// timeout, matching the Cerulean integration.
func New(cfg Config) *Client {
	return &Client{
		cfg: cfg,
		hc:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Config exposes the client's configuration (for status surfaces).
func (c *Client) Config() Config { return c.cfg }

// ResolveEnv resolves a single .env-style value: `infisical://<name>`
// references read the secret from Infisical; anything else is returned
// unchanged. A reference to an unconfigured Infisical, or a read failure, is
// an error — a silently-empty credential would open the S3 endpoint.
func (c *Client) ResolveEnv(ctx context.Context, value string) (string, error) {
	name, ok := Ref(value)
	if !ok {
		return value, nil
	}
	if !c.cfg.Enabled() {
		return "", fmt.Errorf(
			"value %q references Infisical but %s/%s/%s are not configured",
			value, EnvVarAddr, EnvVarToken, EnvVarWorkspaceID,
		)
	}
	return c.ReadSecret(ctx, name)
}

// ReadSecret fetches a single secret's value.
func (c *Client) ReadSecret(ctx context.Context, name string) (string, error) {
	u := c.secretURL(name, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("infisical read %s: %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("infisical read %s failed (HTTP %d): %s",
			name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Secret struct {
			SecretValue string `json:"secretValue"`
		} `json:"secret"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("infisical read %s: bad response: %w", name, err)
	}
	if out.Secret.SecretValue == "" && !bytes.Contains(body, []byte("secretValue")) {
		return "", fmt.Errorf("infisical secret not found: %s", name)
	}
	return out.Secret.SecretValue, nil
}

// WriteSecret upserts a secret value (shared type, root path).
func (c *Client) WriteSecret(ctx context.Context, name, value string) error {
	if !c.cfg.Enabled() {
		return fmt.Errorf("infisical not configured (%s/%s/%s)",
			EnvVarAddr, EnvVarToken, EnvVarWorkspaceID)
	}
	payload, err := json.Marshal(map[string]string{
		"workspaceId": c.cfg.WorkspaceID,
		"environment": c.cfg.Environment,
		"secretPath":  "/",
		"type":        "shared",
		"secretValue": value,
	})
	if err != nil {
		return err
	}
	u := c.secretURL(name, false)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("infisical write %s: %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("infisical write %s failed (HTTP %d): %s",
			name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// Mirror pushes plain env values into Infisical, best-effort: individual
// failures are collected and returned as errs but never abort the caller.
// Values that are already `infisical://` references are skipped. This is the
// on-ramp for the stack's secret flow — after one boot with plain values the
// operator can switch .env to references and the services resolve from
// Infisical.
func (c *Client) Mirror(ctx context.Context, entries map[string]string) (written []string, errs []error) {
	if !c.cfg.Enabled() {
		return written, nil
	}
	for name, value := range entries {
		if value == "" {
			continue
		}
		if _, isRef := Ref(value); isRef {
			continue
		}
		if err := c.WriteSecret(ctx, name, value); err != nil {
			errs = append(errs, err)
			continue
		}
		written = append(written, name)
	}
	return written, errs
}

// Status reports "ok", "not-configured", or an error message — never throws.
func (c *Client) Status(ctx context.Context) string {
	if !c.cfg.Enabled() {
		return "not-configured"
	}
	// A 404 on a probe secret proves auth + connectivity; a network or
	// permission error does not.
	if _, err := c.ReadSecret(ctx, "__probe__"); err != nil {
		if strings.Contains(err.Error(), "(HTTP 404)") {
			return "ok"
		}
		return "error: " + err.Error()
	}
	return "ok"
}

// secretURL builds the raw-secrets endpoint for a name. When query is true the
// workspace/environment query params are appended (read path); the write path
// carries them in the body instead.
func (c *Client) secretURL(name string, query bool) string {
	u := fmt.Sprintf("%s/api/v3/secrets/raw/%s", c.cfg.Addr, url.PathEscape(name))
	if query {
		q := url.Values{}
		q.Set("workspaceId", c.cfg.WorkspaceID)
		q.Set("environment", c.cfg.Environment)
		u += "?" + q.Encode()
	}
	return u
}