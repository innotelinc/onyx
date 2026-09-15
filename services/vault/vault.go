// Package vault is the ONYX client for Cerulean Vault (HashiCorp Vault, KV v2),
// the platform's SecretOps layer: the job is to keep real credentials out of
// .env and out of the compose file, resolved at startup from the one store the
// platform runs.
//
// .env values may be plain text or `vault://<mount>/<path>#<key>` references:
//
//	S3_ACCESS_KEY=vault://cerulean/onyx#S3_ACCESS_KEY
//	CERULEAN_API_TOKEN=vault://cerulean/onyx/api#CERULEAN_API_TOKEN
//
// A reference is resolved at startup through the KV v2 API, so a misconfigured
// store fails loudly instead of opening the S3 endpoint with an empty key.
// Plain values pass through unchanged, which is what makes the migration
// one-sided: switch a value in .env and the service resolves it, no code change.
//
// Environment contract (the same one Olympus's vault-bootstrap.py writes, and
// the one scripts/vault-migrate.py targets):
//
//	VAULT_ADDR          base URL, e.g. http://vault:8200
//	VAULT_TOKEN         a token scoped to this product's own path…
//	VAULT_TOKEN_FILE    …or a file containing one (preferred; the Vault CLI
//	                    reads the same file)
//	VAULT_PREFIX        KV v2 mount point, default "cerulean"
//	VAULT_PATH          path under the mount for THIS product, default "onyx"
//	VAULT_NAMESPACE     Enterprise namespaces; unused on OSS Vault
//	VAULT_SKIP_VERIFY   "1" to accept a self-signed certificate
//	VAULT_CACERT        CA bundle to pin instead
//
// Never the root token and never the mount-wide `cerulean` policy: the
// platform mints a path-scoped `<product>` policy, delivered as
// ./data/vault/token/<product>.token. See docs/stack.md.
package vault

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	// EnvVar* are the env keys this package reads. Keep them in one place so
	// every service wires the same contract.
	EnvVarAddr       = "VAULT_ADDR"
	EnvVarToken      = "VAULT_TOKEN"
	EnvVarTokenFile  = "VAULT_TOKEN_FILE"
	EnvVarPrefix     = "VAULT_PREFIX"
	EnvVarPath       = "VAULT_PATH"
	EnvVarNamespace  = "VAULT_NAMESPACE"
	EnvVarSkipVerify = "VAULT_SKIP_VERIFY"
	EnvVarCACert     = "VAULT_CACERT"

	// DefaultPrefix is Cerulean's KV v2 mount.
	DefaultPrefix = "cerulean"
	// DefaultPath is this product's path under the mount.
	DefaultPath = "onyx"

	refPrefix = "vault://"
)

// Config is the runtime secret-store configuration, sourced from the
// environment. Services construct it once at startup.
type Config struct {
	Addr       string // base URL, no trailing slash
	Token      string // inline token, when the deployment uses one
	TokenFile  string // file holding the token; preferred over Token
	Prefix     string // KV v2 mount point
	Path       string // this product's path under the mount
	Namespace  string // X-Vault-Namespace, empty on OSS Vault
	SkipVerify bool
	CACert     string
}

// ConfigFromEnv builds Config from the VAULT_* environment contract. A missing
// mount or path falls back to Cerulean's defaults rather than disabling the
// store, because a deployment that set VAULT_ADDR and a token has already said
// which store it means.
func ConfigFromEnv() Config {
	cfg := Config{
		Addr:      strings.TrimRight(strings.TrimSpace(os.Getenv(EnvVarAddr)), "/"),
		Token:     strings.TrimSpace(os.Getenv(EnvVarToken)),
		TokenFile: strings.TrimSpace(os.Getenv(EnvVarTokenFile)),
		Prefix:    strings.Trim(strings.TrimSpace(os.Getenv(EnvVarPrefix)), "/"),
		Path:      strings.Trim(strings.TrimSpace(os.Getenv(EnvVarPath)), "/"),
		Namespace: strings.TrimSpace(os.Getenv(EnvVarNamespace)),
		CACert:    strings.TrimSpace(os.Getenv(EnvVarCACert)),
	}
	if cfg.Prefix == "" {
		cfg.Prefix = DefaultPrefix
	}
	if cfg.Path == "" {
		cfg.Path = DefaultPath
	}
	switch strings.ToLower(os.Getenv(EnvVarSkipVerify)) {
	case "1", "true", "yes":
		cfg.SkipVerify = true
	}
	return cfg
}

// Enabled reports whether a working Vault client is configured. The path is not
// checked here: a deployment may only learn its path is unreadable by asking
// (that is what Status is for), and a missing path is a resolved default.
func (c Config) Enabled() bool {
	return c.Addr != "" && (c.Token != "" || c.TokenFile != "")
}

// Ref is a parsed `vault://<mount>/<path>#<key>` reference. Path may be empty
// when the reference names a mount only, and Key may be empty when the caller
// wants the secret's first value (a single-key entry).
type Ref struct {
	Mount string
	Path  string
	Key   string
}

// ParseRef parses a `vault://` reference. Returns ok=false for anything that is
// not a reference, including a bare `vault://` with no location.
//
// The first path segment is the mount, so `vault://cerulean/onyx/api#TOKEN`
// reads the `TOKEN` key of `onyx/api` on the `cerulean` mount. A one-segment
// reference (`vault://onyx#KEY`) has no mount in it — Cerulean's own resolver
// treats that as a path under its configured prefix, which is the same thing
// with the prefix supplied.
func ParseRef(value string) (Ref, bool) {
	if !strings.HasPrefix(value, refPrefix) {
		return Ref{}, false
	}
	location, key, _ := strings.Cut(value[len(refPrefix):], "#")
	location = strings.Trim(strings.TrimSpace(location), "/")
	if location == "" {
		return Ref{}, false
	}
	mount, path, _ := strings.Cut(location, "/")
	ref := Ref{
		Mount: strings.TrimSpace(mount),
		Path:  strings.Trim(strings.TrimSpace(path), "/"),
		Key:   strings.TrimSpace(key),
	}
	if ref.Mount == "" {
		return Ref{}, false
	}
	return ref, true
}

// Client talks to Vault. Construct with New; the zero value is not usable.
type Client struct {
	cfg Config
	hc  *http.Client
}

// New returns a client for cfg. The HTTP client carries a 15 s per-request
// timeout, matching the other platform service clients.
func New(cfg Config) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.SkipVerify || cfg.CACert != "" {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // opting out is the explicit point
		if cfg.SkipVerify {
			tlsConfig.InsecureSkipVerify = true
		}
		if cfg.CACert != "" {
			if pem, err := os.ReadFile(cfg.CACert); err == nil {
				pool := x509.NewCertPool()
				if pool.AppendCertsFromPEM(pem) {
					tlsConfig.RootCAs = pool
				}
			}
		}
		transport.TLSClientConfig = tlsConfig
	}
	return &Client{
		cfg: cfg,
		hc:  &http.Client{Timeout: 15 * time.Second, Transport: transport},
	}
}

// Config exposes the client's configuration (for status surfaces).
func (c *Client) Config() Config { return c.cfg }

// token returns the configured token, reading the token file when the inline
// token is empty — the order the Vault CLI uses.
func (c *Client) token() (string, error) {
	if c.cfg.Token != "" {
		return c.cfg.Token, nil
	}
	if c.cfg.TokenFile == "" {
		return "", fmt.Errorf("no Vault token: set %s, or %s to a file containing one",
			EnvVarToken, EnvVarTokenFile)
	}
	raw, err := os.ReadFile(c.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("reading %s (%s): %w", EnvVarTokenFile, c.cfg.TokenFile, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("%s (%s) is empty", EnvVarTokenFile, c.cfg.TokenFile)
	}
	return token, nil
}

// ResolveEnv resolves a single .env-style value. A `vault://` reference reads the
// secret from Cerulean Vault; anything else is returned unchanged (that is the
// plain-value case, and the reason `.env` can hold either).
//
// Cerulean Vault is the platform's only secret store, so there is deliberately
// no second scheme here: a value that still carries a legacy reference is not a
// secret to look up, it is a deployment that has not finished moving.
//
// A reference to an unconfigured or unreachable store is an error, never an
// empty string: the callers resolve credentials that gate an endpoint, and a
// silently-empty one is worse than a service that refuses to start.
func (c *Client) ResolveEnv(ctx context.Context, value string) (string, error) {
	ref, ok := ParseRef(value)
	if !ok {
		return value, nil
	}
	if !c.cfg.Enabled() {
		return "", fmt.Errorf(
			"value %q references Cerulean Vault but %s and %s/%s are not configured",
			value, EnvVarAddr, EnvVarToken, EnvVarTokenFile,
		)
	}
	secret, err := c.ReadSecret(ctx, ref.Mount, ref.Path)
	if err != nil {
		return "", err
	}
	if ref.Key != "" {
		got, ok := secret[ref.Key]
		if !ok {
			return "", fmt.Errorf("vault secret %s/%s has no key %q", ref.Mount, ref.Path, ref.Key)
		}
		return got, nil
	}
	for _, k := range sortedKeys(secret) {
		return secret[k], nil
	}
	return "", fmt.Errorf("vault secret %s/%s is empty", ref.Mount, ref.Path)
}

// ReadSecret reads every key of a KV v2 secret at <mount>/<path>. A missing
// secret is an error naming the path, because every caller here is resolving a
// credential it cannot invent.
func (c *Client) ReadSecret(ctx context.Context, mount, path string) (map[string]string, error) {
	token, err := c.token()
	if err != nil {
		return nil, err
	}
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if mount == "" || path == "" {
		return nil, fmt.Errorf("vault path is incomplete: mount=%q path=%q", mount, path)
	}

	url := fmt.Sprintf("%s/v1/%s/data/%s", c.cfg.Addr, mount, escapePath(path))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Accept", "application/json")
	if c.cfg.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", c.cfg.Namespace)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault read %s/%s: %w", mount, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("vault secret not found: %s/%s", mount, path)
	case http.StatusForbidden:
		return nil, fmt.Errorf(
			"vault read %s/%s denied (HTTP 403) — the token's policy does not cover it: %s",
			mount, path, strings.TrimSpace(string(body)),
		)
	default:
		return nil, fmt.Errorf("vault read %s/%s failed (HTTP %d): %s",
			mount, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// KV v2 nests the payload under data.data. A response without that nesting
	// means the mount is answered by something that is not KV v2 (or a v1
	// mount), and reading it as if it were would hand back nothing at all.
	var envelope struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("vault read %s/%s: bad response: %w", mount, path, err)
	}
	if envelope.Data.Data == nil {
		return nil, fmt.Errorf(
			"vault read %s/%s did not answer as KV v2 (no data.data) — point %s at the KV v2 mount",
			mount, path, EnvVarPrefix,
		)
	}
	out := make(map[string]string, len(envelope.Data.Data))
	for k, v := range envelope.Data.Data {
		if s, ok := v.(string); ok {
			out[k] = s
			continue
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out[k] = string(encoded)
	}
	return out, nil
}

// Get reads one key from this product's own secret — <VAULT_PREFIX>/<VAULT_PATH>
// — which is what a service wants when it is not resolving a .env reference.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	mount, path, err := c.location()
	if err != nil {
		return "", err
	}
	secret, err := c.ReadSecret(ctx, mount, path)
	if err != nil {
		return "", err
	}
	got, ok := secret[key]
	if !ok {
		return "", fmt.Errorf("vault secret %s/%s has no key %q", mount, path, key)
	}
	return got, nil
}

// Status reports "ok", "not-configured", or an error message — never throws.
//
// The probe is the stack's OWN path, not a shared one: a path-scoped token
// cannot read an unrelated path, and an operator reading "error: HTTP 403" for
// a store that is working perfectly would be misled. A 404 is a pass — it
// proves the data endpoint and the token both work, which is all this reports.
func (c *Client) Status(ctx context.Context) string {
	if !c.cfg.Enabled() {
		return "not-configured"
	}
	mount, path, err := c.location()
	if err != nil {
		return "error: " + err.Error()
	}
	if _, err := c.ReadSecret(ctx, mount, path); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return "ok"
		}
		return "error: " + err.Error()
	}
	return "ok"
}

// location returns the configured mount and path.
func (c *Client) location() (mount, path string, err error) {
	if c.cfg.Addr == "" {
		return "", "", fmt.Errorf("%s is not set", EnvVarAddr)
	}
	if c.cfg.Prefix == "" || c.cfg.Path == "" {
		return "", "", fmt.Errorf(
			"no vault location: set %s/%s (defaults %s/%s)",
			EnvVarPrefix, EnvVarPath, DefaultPrefix, DefaultPath,
		)
	}
	return c.cfg.Prefix, c.cfg.Path, nil
}

// escapePath escapes each segment of a KV path but keeps the separators, which
// URL-escaping the whole string would eat.
func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// sortedKeys gives ResolveEnv a deterministic pick when the caller asked for no
// particular key. Small enough to earn its keep over importing sort for one
// line elsewhere.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
