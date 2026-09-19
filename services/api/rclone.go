package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path"
	"strings"
	"time"
)

// rcloneConfigPath is the single remote catalog. The API writes it (the Shares
// page's cloud-storage setup) and backupd reads it for remote backup targets,
// so both containers see the same remotes without a restart: rclone reads the
// file on every invocation. Compose mounts it read-write into onyx-api.
const rcloneConfigPath = "/etc/rclone/rclone.conf"

// rcloneProvider is one backend the Shares page can set up. Field names are
// rclone's own option names, so a value the operator types is passed straight
// through — but only for keys listed here, which keeps the command line a
// closed set (no arbitrary rclone options, no shell).
type rcloneProvider struct {
	Type     string   `json:"type"`
	Label    string   `json:"label"`
	Fields   []string `json:"fields"`
	Required []string `json:"required"`
	// OAuth backends cannot be finished from stored credentials: the account
	// owner has to approve access once in a browser. The remote is created
	// here and the response tells the operator how to complete it.
	OAuth bool   `json:"oauth"`
	Hint  string `json:"hint,omitempty"`
}

func rcloneProviders() []rcloneProvider {
	return []rcloneProvider{
		{
			Type:     "s3",
			Label:    "Amazon S3 or S3-compatible",
			Fields:   []string{"provider", "access_key_id", "secret_access_key", "region", "endpoint"},
			Required: []string{"access_key_id", "secret_access_key"},
			Hint:     "provider selects the S3 flavour (AWS, Minio, Wasabi, Cloudflare, ...); set endpoint for MinIO or another self-hosted store.",
		},
		{Type: "b2", Label: "Backblaze B2", Fields: []string{"account", "key"}, Required: []string{"account", "key"}},
		{Type: "sftp", Label: "SFTP server", Fields: []string{"host", "user", "port", "pass", "key_file"}, Required: []string{"host", "user"}},
		{Type: "smb", Label: "SMB / CIFS share", Fields: []string{"host", "user", "domain", "pass"}, Required: []string{"host"}},
		{Type: "webdav", Label: "WebDAV", Fields: []string{"url", "vendor", "user", "pass"}, Required: []string{"url"}},
		{Type: "ftp", Label: "FTP", Fields: []string{"host", "user", "port", "pass"}, Required: []string{"host"}},
		{
			Type:     "google cloud storage",
			Label:    "Google Cloud Storage",
			Fields:   []string{"service_account_file", "project_number"},
			Required: []string{"service_account_file"},
			Hint:     "service_account_file is a path inside the onyx-api container; mount the key next to rclone.conf to use it.",
		},
		{Type: "azureblob", Label: "Azure Blob Storage", Fields: []string{"account", "key"}, Required: []string{"account", "key"}},
		{Type: "mega", Label: "MEGA", Fields: []string{"user", "pass"}, Required: []string{"user", "pass"}},
		// OAuth backends. client_id/client_secret are optional: left empty,
		// rclone uses its own registered application. Filling them in is for
		// organisations that want the consent screen under their own name, and
		// then the same pair has to be given to `rclone authorize`.
		{
			Type:   "drive",
			Label:  "Google Drive",
			Fields: []string{"client_id", "client_secret"},
			OAuth:  true,
			Hint:   "Google Drive is approved in a browser. On a machine with a browser run `rclone authorize drive`, then paste the token here.",
		},
		{
			Type:   "onedrive",
			Label:  "Microsoft OneDrive",
			Fields: []string{"client_id", "client_secret"},
			OAuth:  true,
			Hint:   "OneDrive (personal or Microsoft 365) is approved in a browser. On a machine with a browser run `rclone authorize onedrive`, then paste the token here.",
		},
		{
			Type:   "sharepoint",
			Label:  "Microsoft SharePoint",
			Fields: []string{"client_id", "client_secret", "site_url"},
			OAuth:  true,
			Hint:   "SharePoint is approved in a browser. On a machine with a browser run `rclone authorize sharepoint`, then paste the token here. Set site_url to the document library.",
		},
		{
			Type:   "dropbox",
			Label:  "Dropbox",
			Fields: []string{"client_id", "client_secret"},
			OAuth:  true,
			Hint:   "Dropbox is approved in a browser. On a machine with a browser run `rclone authorize dropbox`, then paste the token here.",
		},
		{
			Type:   "box",
			Label:  "Box",
			Fields: []string{"client_id", "client_secret"},
			OAuth:  true,
			Hint:   "Box is approved in a browser. On a machine with a browser run `rclone authorize box`, then paste the token here.",
		},
		{
			Type:   "pcloud",
			Label:  "pCloud",
			Fields: []string{"client_id", "client_secret"},
			OAuth:  true,
			Hint:   "pCloud is approved in a browser. On a machine with a browser run `rclone authorize pcloud`, then paste the token here.",
		},
	}
}

// oauthProviderTypes is the set of backends finished with a stored token, so a
// remote's row can offer a Connect action without a second lookup from the UI.
func oauthProviderTypes() map[string]bool {
	out := map[string]bool{}
	for _, p := range rcloneProviders() {
		if p.OAuth {
			out[p.Type] = true
		}
	}
	return out
}

func providerByType(name string) (rcloneProvider, bool) {
	for _, p := range rcloneProviders() {
		if p.Type == strings.ToLower(strings.TrimSpace(name)) {
			return p, true
		}
	}
	return rcloneProvider{}, false
}

func runRclone(ctx context.Context, args ...string) (string, error) {
	full := append(append([]string{}, args...), "--config", rcloneConfigPath)
	out, err := exec.CommandContext(ctx, "rclone", full...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("%s", text)
	}
	return text, nil
}

// networkBounds are appended to every probe that talks to a provider. rclone
// retries a failing request three times with a long connect timeout by default,
// which turns a rejected token or an unreachable host into a console that looks
// like it has hung; the probe has to come back with an answer instead.
var networkBounds = []string{"--retries", "1", "--low-level-retries", "1", "--contimeout", "10s", "--timeout", "15s"}

// remoteNames lists the configured remotes (the names rclone calls them).
func remoteNames(ctx context.Context) ([]string, error) {
	output, err := runRclone(ctx, "listremotes")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0)
	for _, line := range strings.Split(output, "\n") {
		if line = strings.TrimSpace(line); strings.HasSuffix(line, ":") {
			names = append(names, strings.TrimSuffix(line, ":"))
		}
	}
	return names, nil
}

// handleRcloneRemotes serves GET /api/v1/storage/remotes — the configured
// remote catalog, with each remote's backend type for the Shares page. A
// missing config is a valid unconfigured state, not an error.
func (s *server) handleRcloneRemotes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	names, err := remoteNames(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "remotes": []string{}, "details": map[string]string{}, "error": err.Error()})
		return
	}
	// `config dump` is the only way to read a remote's type back; listremotes
	// only yields names.
	types := map[string]string{}
	if dump, derr := runRclone(ctx, "config", "dump"); derr == nil {
		var parsed map[string]map[string]any
		if json.Unmarshal([]byte(dump), &parsed) == nil {
			for name, section := range parsed {
				if t, ok := section["type"].(string); ok {
					types[name] = t
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":     len(names) > 0,
		"remotes":        names,
		"details":        types,
		"providers":      rcloneProviders(),
		"oauth_redirect": oauthRedirectTypes(),
	})
}

// handleRcloneProviders serves GET /api/v1/storage/providers — the closed set
// of backends the cloud-storage form offers.
func (s *server) handleRcloneProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": rcloneProviders(),
		// Which OAuth backends the console can sign in for itself, as opposed to
		// the ones that need a token pasted from `rclone authorize` elsewhere.
		"oauth_redirect": oauthRedirectTypes(),
	})
}

type createRemoteBody struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Params map[string]string `json:"params"`
}

// validateRemoteName keeps a remote name safe as an argv element: rclone
// rejects ':' and '/', and a leading '-' would be read as a flag.
func validateRemoteName(name string) error {
	if name == "" || len(name) > 64 {
		return fmt.Errorf("remote name must be 1-64 characters")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("remote name must not start with '-'")
	}
	for _, c := range name {
		if !(c == '.' || c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return fmt.Errorf("remote name may only contain letters, numbers, '.', '_' and '-'")
		}
	}
	return nil
}

// handleCreateRemote serves POST /api/v1/storage/remotes. It writes one remote
// into the shared rclone config so the Shares page can publish a folder to a
// cloud or remote server and backups can target it.
//
// Secret values are handed to rclone untouched, because obscuring them is
// rclone's job and not ours: `rclone config create` obscures the options a
// backend marks as passwords (an SFTP/SMB/WebDAV/FTP `pass`) and stores the rest
// verbatim — an S3 `secret_access_key` or a B2 `key` goes to the provider
// exactly as written. Pre-obscuring here did neither: it corrupted the raw
// fields, so an S3 remote built from the Shares page failed every request with
// SignatureDoesNotMatch and never authenticated.
func (s *server) handleCreateRemote(w http.ResponseWriter, r *http.Request) {
	var body createRemoteBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if err := validateRemoteName(body.Name); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	provider, ok := providerByType(body.Type)
	if !ok {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("unknown provider %q", body.Type)})
		return
	}
	// Only the provider's own options may be set; anything else is rejected
	// rather than passed through to rclone.
	allowed := map[string]bool{}
	for _, field := range provider.Fields {
		allowed[field] = true
	}
	for key := range body.Params {
		if !allowed[key] {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("%s does not accept the option %q", provider.Type, key)})
			return
		}
	}
	for _, key := range provider.Required {
		if strings.TrimSpace(body.Params[key]) == "" {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("%s requires %s", provider.Type, key)})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := remoteNames(ctx); err != nil {
		// A missing config file is normal; a read-only one is not, and the
		// operator needs to know which it is.
		if strings.Contains(err.Error(), "read-only") || strings.Contains(err.Error(), "permission denied") {
			writeEnvelope(w, http.StatusConflict, apiError{Code: "failed_precondition", Message: "the rclone config is not writable by onyx-api: give the mount at " + rcloneConfigPath + " write permission for the container user"})
			return
		}
	}
	args := []string{"config", "create", body.Name, provider.Type, "--non-interactive"}
	for _, field := range provider.Fields {
		value := strings.TrimSpace(body.Params[field])
		if value == "" {
			continue
		}
		if len(value) > 1024 {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("%s is too long", field)})
			return
		}
		if strings.ContainsAny(value, "\n\r\x00") || strings.HasPrefix(value, "-") {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("%s contains characters rclone cannot take on the command line", field)})
			return
		}
		args = append(args, field, value)
	}
	if output, err := runRclone(ctx, args...); err != nil {
		writeEnvelope(w, http.StatusBadGateway, apiError{Code: "internal", Message: "rclone config create: " + output})
		return
	}
	response := map[string]any{"name": body.Name, "type": provider.Type, "oauth": provider.OAuth}
	if provider.OAuth {
		response["next_step"] = fmt.Sprintf("Run `rclone config reconnect %s:` on the host to approve access in a browser.", body.Name)
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *server) handleDeleteRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateRemoteName(name); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if output, err := runRclone(ctx, "config", "delete", name); err != nil {
		writeEnvelope(w, http.StatusBadGateway, apiError{Code: "internal", Message: "rclone config delete: " + output})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

// handleCheckRemote serves POST /api/v1/storage/remotes/{name}/check — a
// reachability probe the Shares page runs after setup, so a wrong key or a
// typo'd host surfaces in the UI instead of during a clone or backup.
type remoteTokenBody struct {
	Token        string `json:"token"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// extractRcloneToken pulls the token JSON out of whatever `rclone authorize`
// printed. rclone wraps it in markers:
//
//	Paste the following into your remote machine --->
//	{...}
//	<---End paste
//
// so the JSON object is taken from the first '{' to the last '}' and compacted
// to one line before it is handed to rclone as an argv element.
func extractRcloneToken(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return "", fmt.Errorf("no token found: paste the JSON rclone authorize printed (it starts with '{')")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(text[start:end+1])); err != nil {
		return "", fmt.Errorf("the pasted token is not valid JSON: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(compact.String()), &parsed); err != nil {
		return "", fmt.Errorf("the pasted token is not valid JSON: %v", err)
	}
	if _, ok := parsed["access_token"]; !ok {
		if _, ok := parsed["refresh_token"]; !ok {
			return "", fmt.Errorf("the pasted JSON has no access_token or refresh_token — it is not an rclone token")
		}
	}
	return compact.String(), nil
}

// handleSetRemoteToken serves POST /api/v1/storage/remotes/{name}/token: the
// step that actually connects a Google Drive / OneDrive / Dropbox account.
//
// OAuth backends cannot be finished from the console alone — the account owner
// has to approve access in a browser, and the callback lands on 127.0.0.1 of
// whichever machine ran the approval. So the console takes rclone's own answer
// and stores it: the operator runs `rclone authorize <type>` where a browser
// works (or signs in through the provider's own OAuth app), pastes the JSON the
// command prints, and rclone is told `config update NAME token <json>`.
//
// Storing the token is not enough on its own — a token that has been revoked, or
// copied from an app with a different client id, looks exactly like a good one —
// so the remote is probed immediately and the result is reported back with the
// same shape the Check button uses.
func (s *server) handleSetRemoteToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateRemoteName(name); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	var body remoteTokenBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	token, err := extractRcloneToken(body.Token)
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	opts := [][2]string{}
	for _, pair := range [][2]string{{"client_id", body.ClientID}, {"client_secret", body.ClientSecret}} {
		value := strings.TrimSpace(pair[1])
		if value == "" {
			continue
		}
		if len(value) > 1024 || strings.ContainsAny(value, "\n\r\x00") || strings.HasPrefix(value, "-") {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: pair[0] + " is not usable"})
			return
		}
		opts = append(opts, [2]string{pair[0], value})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	if names, err := remoteNames(ctx); err != nil || !containsString(names, name) {
		writeEnvelope(w, http.StatusNotFound, apiError{Code: "not_found", Message: fmt.Sprintf("remote %q is not configured — save the target first", name)})
		return
	}
	// --non-interactive matters as much as the token itself: without it rclone
	// asks "Already have a token - refresh?" and, when it cannot reach the
	// provider from here, starts its own browser authorization flow and waits —
	// the request never answers. The token is written either way, so the flag
	// turns a hang into the probe that follows.
	args := []string{"config", "update", name, "token", token, "--non-interactive"}
	for _, opt := range opts {
		args = append(args, opt[0], opt[1])
	}
	// Both halves are bounded: `config update` on an OAuth backend may try to
	// refresh the token it is given, and the account probe talks to the
	// provider. A rejected token or an unreachable provider has to come back as
	// an answer, not as a console that waits — rclone retries by default, so the
	// retries are cut to one and the timeouts are set.
	updateCtx, updateCancel := context.WithTimeout(ctx, 30*time.Second)
	defer updateCancel()
	if output, err := runRclone(updateCtx, append(args, networkBounds...)...); err != nil {
		writeEnvelope(w, http.StatusBadGateway, apiError{Code: "internal", Message: "rclone config update token: " + output})
		return
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer probeCancel()
	output, probeErr := runRclone(probeCtx, append([]string{"lsd", name + ":", "--max-depth", "1"}, networkBounds...)...)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   name,
		"stored": true,
		"ok":     probeErr == nil,
		"detail": output,
	})
}

func (s *server) handleCheckRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateRemoteName(name); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	output, err := runRclone(ctx, append([]string{"lsd", name + ":", "--max-depth", "1"}, networkBounds...)...)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "ok": false, "detail": output})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "ok": true, "detail": output})
}

type cloneRemoteBody struct {
	// Source is relative to the storage root (/mnt/onyx) — the same address
	// space as the share path picker.
	Source string `json:"source"`
	// Remote is a configured remote name; Dest is the folder inside it.
	Remote string `json:"remote"`
	Dest   string `json:"dest"`
}

// handleCloneToRemote serves POST /api/v1/storage/clone: copy a storage folder
// (or file) to a configured remote. This is the "clone a pool to the cloud"
// action on the Shares page. rclone runs with explicit argv and a bounded
// timeout; large copies stream until they finish or the request is cancelled.
func (s *server) handleCloneToRemote(w http.ResponseWriter, r *http.Request) {
	var body cloneRemoteBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	abs, rel, err := resolveFilePath(s.filesRoot, body.Source)
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if rel == "" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "choose a folder inside mounted storage to clone"})
		return
	}
	if err := validateRemoteName(strings.TrimSpace(body.Remote)); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	remote := strings.TrimSpace(body.Remote)
	dest := strings.Trim(strings.TrimSpace(body.Dest), "/")
	if len(dest) > 512 || strings.ContainsAny(dest, "\n\r\x00") || strings.HasPrefix(dest, "-") {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "destination folder is not usable"})
		return
	}
	if dest != "" {
		dest = path.Clean(dest)
	}
	target := remote + ":"
	if dest != "" && dest != "." {
		target += dest
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if names, err := remoteNames(ctx); err != nil || !containsString(names, remote) {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("remote %q is not configured", remote)})
		return
	}
	// --create-empty-src-dirs keeps an empty folder structure intact; the copy
	// is additive (rclone copy never deletes at the destination).
	output, err := runRclone(ctx, "copy", abs, target, "--create-empty-src-dirs", "--stats-one-line", "-v")
	if err != nil {
		writeEnvelope(w, http.StatusBadGateway, apiError{Code: "internal", Message: "rclone copy: " + output})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source":   rel,
		"target":   target,
		"detail":   lastLines(output, 5),
		"finished": true,
	})
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// lastLines keeps the tail of rclone's verbose output: the summary line (bytes
// and file counts) is what an operator actually wants to see.
func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
