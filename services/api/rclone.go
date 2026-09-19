package main

import (
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
		{
			Type:  "drive",
			Label: "Google Drive",
			OAuth: true,
			Hint:  "Google Drive needs a one-time browser approval: run `rclone config reconnect NAME:` on the host.",
		},
		{
			Type:  "onedrive",
			Label: "Microsoft OneDrive",
			OAuth: true,
			Hint:  "OneDrive needs a one-time browser approval: run `rclone config reconnect NAME:` on the host.",
		},
		{
			Type:  "dropbox",
			Label: "Dropbox",
			OAuth: true,
			Hint:  "Dropbox needs a one-time browser approval: run `rclone config reconnect NAME:` on the host.",
		},
		{
			Type:  "box",
			Label: "Box",
			OAuth: true,
			Hint:  "Box needs a one-time browser approval: run `rclone config reconnect NAME:` on the host.",
		},
		{
			Type:  "pcloud",
			Label: "pCloud",
			OAuth: true,
			Hint:  "pCloud needs a one-time browser approval: run `rclone config reconnect NAME:` on the host.",
		},
	}
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
		"configured": len(names) > 0,
		"remotes":    names,
		"details":    types,
		"providers":  rcloneProviders(),
	})
}

// handleRcloneProviders serves GET /api/v1/storage/providers — the closed set
// of backends the cloud-storage form offers.
func (s *server) handleRcloneProviders(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": rcloneProviders()})
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
func (s *server) handleCheckRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateRemoteName(name); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	output, err := runRclone(ctx, "lsd", name+":", "--max-depth", "1")
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
