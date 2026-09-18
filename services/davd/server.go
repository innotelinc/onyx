// onyx-davd serves the WebDAV protocol for WebDAV-enabled shares
// (docs/design/05#6 WebDAV row): a Go WebDAV server that renders the share
// table onyx-shared generates, terminates no TLS itself (HTTPS is the edge's
// job) and authenticates nobody itself — it trusts the identity header the
// gateway sets after it has authenticated the session or app token.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/net/webdav"
)

// shareNameRe matches the share names onyx-core accepts, so a name is always
// safe to place in a URL path without escaping.
var shareNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// defaultConfigPath is where onyx-core/privd write the rendered davd.conf.
const defaultConfigPath = "/etc/onyx/conf.d/davd.conf"

// maxConfigBytes caps the config read: the file is machine-generated and small,
// so a huge one is a mistake worth failing on rather than buffering.
const maxConfigBytes = 1 << 20

// share is one WebDAV-visible share: a URL segment mapped onto a directory.
type share struct {
	Name     string
	Path     string
	Readonly bool
}

// config is the parsed davd.conf. Only the fields onyx-shared renders are
// understood; an unknown key is an error rather than a silent default, because
// a config that says something the daemon ignores is a bug worth surfacing.
type config struct {
	Listen         string
	TLS            string
	Auth           string
	IdentityHeader string
	Shares         []share
}

// authModeGateway is the default: the gateway authenticated the caller and
// passed the identity in a header. authModeNone is loopback-only and exists for
// a local smoke test, not for a deployment.
const (
	authModeGateway = "gateway"
	authModeNone    = "none"
)

// parseConfig reads the TOML-subset onyx-shared renders: `[server]`, repeated
// `[[share]]` tables, and `key = "value"` / `key = true` pairs.
func parseConfig(data []byte) (*config, error) {
	cfg := &config{
		Listen:         "127.0.0.1:8081",
		TLS:            "upstream",
		Auth:           authModeGateway,
		IdentityHeader: "X-Onyx-User",
	}

	section := ""
	seen := map[string]bool{}

	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		switch {
		case line == "[[share]]":
			cfg.Shares = append(cfg.Shares, share{})
			section = "share"
			continue
		case line == "[server]":
			section = "server"
			continue
		case strings.HasPrefix(line, "["):
			return nil, fmt.Errorf("line %d: unsupported section %q", n+1, line)
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value, got %q", n+1, line)
		}
		key = strings.TrimSpace(key)
		val, err := parseValue(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n+1, err)
		}

		switch section {
		case "server":
			switch key {
			case "listen":
				cfg.Listen = val
			case "tls":
				cfg.TLS = val
			case "auth":
				cfg.Auth = val
			case "identity_header":
				cfg.IdentityHeader = val
			default:
				return nil, fmt.Errorf("line %d: unknown server key %q", n+1, key)
			}
		case "share":
			if len(cfg.Shares) == 0 {
				return nil, fmt.Errorf("line %d: %q outside a [[share]] table", n+1, key)
			}
			s := &cfg.Shares[len(cfg.Shares)-1]
			switch key {
			case "name":
				s.Name = val
			case "path":
				s.Path = val
			case "readonly":
				s.Readonly = val == "true"
			default:
				return nil, fmt.Errorf("line %d: unknown share key %q", n+1, key)
			}
		default:
			return nil, fmt.Errorf("line %d: %q appears before any section", n+1, key)
		}
	}

	for _, s := range cfg.Shares {
		if seen[s.Name] {
			return nil, fmt.Errorf("duplicate share name %q", s.Name)
		}
		seen[s.Name] = true
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate rejects configurations the daemon must not run with: a share that
// could escape its root, a listener the auth mode cannot protect, or more than
// one share bound to the same name.
func (c *config) validate() error {
	switch c.Auth {
	case authModeGateway, authModeNone:
	default:
		return fmt.Errorf("auth must be %q or %q, got %q", authModeGateway, authModeNone, c.Auth)
	}
	if c.IdentityHeader == "" {
		return errors.New("identity_header must not be empty")
	}

	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q must be host:port: %w", c.Listen, err)
	}
	// Without a gateway in front, an identity header is just something a client
	// can type, so the listener itself has to be the boundary.
	if c.Auth == authModeNone && !isLoopback(host) {
		return fmt.Errorf("auth = %q is only allowed on a loopback listen address, got %q", authModeNone, c.Listen)
	}

	for i := range c.Shares {
		s := &c.Shares[i]
		if !shareNameRe.MatchString(s.Name) {
			return fmt.Errorf("share name %q must match %s", s.Name, shareNameRe)
		}
		clean := path.Clean(s.Path)
		if !strings.HasPrefix(clean, "/") {
			return fmt.Errorf("share %q path %q must be absolute", s.Name, s.Path)
		}
		if clean != s.Path || strings.Contains(s.Path, "..") {
			return fmt.Errorf("share %q path %q must be a clean absolute path", s.Name, s.Path)
		}
	}
	return nil
}

// stripComment removes a trailing `#` comment, ignoring hashes inside a quoted
// value (a path may legitimately contain one).
func stripComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

// parseValue accepts the two value forms the renderer emits: a quoted string
// and a bare boolean. Anything else is an error.
func parseValue(v string) (string, error) {
	if strings.HasPrefix(v, `"`) {
		var s string
		if err := json.Unmarshal([]byte(v), &s); err != nil {
			return "", fmt.Errorf("invalid quoted value %s", v)
		}
		return s, nil
	}
	if v == "true" || v == "false" {
		return v, nil
	}
	return "", fmt.Errorf("unsupported value %s (expected a quoted string or true/false)", v)
}

func loadConfig(path string) (*config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		data = append(data, buf[:n]...)
		if len(data) > maxConfigBytes {
			return nil, fmt.Errorf("%s exceeds %d bytes", path, maxConfigBytes)
		}
		if err != nil {
			break
		}
	}
	return parseConfig(data)
}

// --- HTTP surface ---

// reloadable is the handler the listener serves: a SIGHUP replaces the inner
// handler without dropping the socket, so a share change costs no connections.
type reloadable struct {
	mu     sync.RWMutex
	inner  http.Handler
	shares []share
}

func (r *reloadable) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	inner := r.inner
	r.mu.RUnlock()
	inner.ServeHTTP(w, req)
}

func (r *reloadable) swap(h http.Handler, shares []share) {
	r.mu.Lock()
	r.inner, r.shares = h, shares
	r.mu.Unlock()
}

// currentShares reports the active share table (for the index endpoint).
func (r *reloadable) currentShares() []share {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]share(nil), r.shares...)
}

// newHandler builds the HTTP surface for one config revision: the identity
// check wraps everything, `/webdav/<share>` (the path the UI advertises) mounts
// the share, and `/dav/<share>` is an alias for clients configured by hand.
func newHandler(cfg *config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONStatus(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	for _, s := range cfg.Shares {
		h := newShareHandler(s)
		for _, prefix := range []string{"/webdav/", "/dav/"} {
			mux.Handle(prefix+s.Name+"/", http.StripPrefix(prefix+s.Name, h))
			// A client that opens the collection without the trailing slash
			// gets a redirect to the mounted path rather than a 404.
			mux.HandleFunc("GET "+prefix+s.Name, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, prefix+s.Name+"/", http.StatusMovedPermanently)
			})
		}
	}

	// The index tells the gateway (and an operator) which shares exist without
	// exposing the share contents.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONStatus(w, http.StatusOK, map[string]any{"service": "onyx-davd", "shares": shareList(cfg.Shares)})
	})

	return authenticate(cfg, mux)
}

// shareList is the share table as the index reports it.
func shareList(shares []share) []map[string]any {
	out := make([]map[string]any, 0, len(shares))
	for _, s := range shares {
		out = append(out, map[string]any{"name": s.Name, "path": s.Path, "readonly": s.Readonly})
	}
	return out
}

// newShareHandler mounts one share, wrapping the WebDAV handler with its write
// policy. LockSystem is in-memory: Onyx shares are reached through this single
// process, so there is no second node to coordinate locks with.
func newShareHandler(s share) http.Handler {
	dav := &webdav.Handler{
		FileSystem: webdav.Dir(s.Path),
		LockSystem: webdav.NewMemLS(),
	}
	if !s.Readonly {
		return dav
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWriteMethod(r.Method) {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{
				"error": map[string]any{"code": "permission_denied", "message": "share " + s.Name + " is read-only"},
			})
			return
		}
		dav.ServeHTTP(w, r)
	})
}

// isWriteMethod reports whether a WebDAV method changes the share's contents.
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodDelete, http.MethodPost, http.MethodPatch,
		"MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK", "UNLOCK":
		return true
	}
	return false
}

// authenticate enforces the identity contract. In gateway mode a request
// without the configured identity header is rejected: the header is the only
// proof of who the caller is, and a request that skipped the gateway was never
// authenticated (docs/design/07#1-authentication).
func authenticate(cfg *config, next http.Handler) http.Handler {
	if cfg.Auth == authModeNone {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimSpace(r.Header.Get(cfg.IdentityHeader)) == "" {
			writeJSONStatus(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"code": "unauthenticated", "message": "missing " + cfg.IdentityHeader + " (WebDAV is reached through the Onyx gateway)"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
