package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Hybrid cloud tiering (docs/design/11 §6.6). A bucket is LOCAL, CLOUD (the
// external target is primary and local storage is a cache) or TIERED (local is
// the hot tier, the external target is the cold tier, with sync and eviction).
//
// The cloud side goes through rclone rather than a native S3 client on purpose:
// the platform already ships one remote catalog (the Shares page's cloud
// storage setup, /etc/rclone/rclone.conf) that backupd and onyx-api both use,
// so an operator configures Backblaze B2 or S3 once and object tiering, backups
// and clones all target it. rclone also brings the primitive this feature needs
// to be safe — `rclone check` compares size and checksum of every file, which is
// what makes eviction a verified operation rather than a hopeful one.

// cloudObjectPrefix is the path every Onyx bucket lives under inside a
// configured remote, so pointing a bucket at a remote that already holds
// unrelated data cannot collide with it.
const cloudObjectPrefix = "onyx-objectstore"

// cloudTransport is the cloud-side operations a bucket needs. It is an interface
// so the tiering policy — what gets uploaded, what may be evicted — is testable
// without a network or an rclone install.
type cloudTransport interface {
	// Upload writes one local file to an exact target path.
	Upload(ctx context.Context, localPath, target string) error
	// Copy uploads a whole directory into target, skipping files the target
	// already holds identically.
	Copy(ctx context.Context, localPath, target string) (string, error)
	// Verify reports an error unless every file in localPath exists in target
	// with matching size and checksum. Eviction never proceeds without it.
	Verify(ctx context.Context, localPath, target string) error
	// Fetch downloads one object from target to localPath.
	Fetch(ctx context.Context, target, localPath string) error
	// Remove deletes one object from target.
	Remove(ctx context.Context, target string) error
	// Purge deletes target and everything under it.
	Purge(ctx context.Context, target string) error
	// Size reports the object count and byte total under target.
	Size(ctx context.Context, target string) (int64, int64, error)
}

// remoteLister is an optional transport capability: a transport that can name
// the configured remotes lets CreateBucket refuse a typo instead of creating a
// bucket that fails on its first write.
type remoteLister interface {
	Remotes() ([]string, bool)
}

// rcloneTransport runs the rclone binary. Every call is explicit argv — never a
// shell — and carries the shared config plus a bounded timeout, because a
// hanging cloud call must not wedge the object store.
type rcloneTransport struct {
	bin    string
	config string
	// timeout bounds one rclone invocation. Uploads stream for as long as they
	// need to, so the bound is generous and per call.
	timeout time.Duration
}

func newRcloneTransport(bin, config string) *rcloneTransport {
	return &rcloneTransport{bin: bin, config: config, timeout: 30 * time.Minute}
}

func (t *rcloneTransport) run(ctx context.Context, args ...string) (string, error) {
	if t.bin == "" {
		return "", fmt.Errorf("cloud transport is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	full := append(append([]string{}, args...), "--config", t.config)
	out, err := exec.CommandContext(ctx, t.bin, full...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("rclone %s: %s", args[0], lastLine(text))
	}
	return text, nil
}

func (t *rcloneTransport) Upload(ctx context.Context, localPath, target string) error {
	_, err := t.run(ctx, "copyto", localPath, target)
	return err
}

func (t *rcloneTransport) Copy(ctx context.Context, localPath, target string) (string, error) {
	// --create-empty-src-dirs keeps an empty prefix meaningful; rclone copy
	// never deletes at the destination, so a copy is always additive.
	return t.run(ctx, "copy", localPath, target, "--create-empty-src-dirs", "--stats-one-line", "-v")
}

func (t *rcloneTransport) Verify(ctx context.Context, localPath, target string) error {
	// --one-way: every local file must exist remotely (extras in the cloud are
	// fine — they are cold copies this tier no longer holds).
	_, err := t.run(ctx, "check", localPath, target, "--one-way")
	return err
}

func (t *rcloneTransport) Fetch(ctx context.Context, target, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return err
	}
	_, err := t.run(ctx, "copyto", target, localPath)
	return err
}

func (t *rcloneTransport) Remove(ctx context.Context, target string) error {
	_, err := t.run(ctx, "deletefile", target)
	return err
}

func (t *rcloneTransport) Purge(ctx context.Context, target string) error {
	_, err := t.run(ctx, "purge", target)
	return err
}

// Size parses `rclone size --json`, which reports {count, bytes}.
func (t *rcloneTransport) Size(ctx context.Context, target string) (int64, int64, error) {
	out, err := t.run(ctx, "size", target, "--json")
	if err != nil {
		return 0, 0, err
	}
	var parsed struct {
		Count int64 `json:"count"`
		Bytes int64 `json:"bytes"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return 0, 0, fmt.Errorf("parse rclone size output: %w", err)
	}
	return parsed.Count, parsed.Bytes, nil
}

// Remotes lists the remote names in the shared catalog. known is false when the
// catalog cannot be read at all — a state the caller must treat as "unknown"
// rather than "absent", so a misread config never blocks a legitimate bucket.
func (t *rcloneTransport) Remotes() ([]string, bool) {
	if t.bin == "" {
		return nil, false
	}
	out, err := t.run(context.Background(), "listremotes")
	if err != nil {
		return nil, false
	}
	names := make([]string, 0)
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasSuffix(line, ":") {
			names = append(names, strings.TrimSuffix(line, ":"))
		}
	}
	return names, true
}

// lastLine keeps the final line of rclone's output: it carries the error or the
// transfer summary, where the earlier lines are per-file progress.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// cloudTargetFor is where one bucket's objects live inside its configured
// target. validateCloudTarget has already rejected a malformed value.
//
// A remote keeps its path right after the colon (`b2-archive:onyx-objectstore/
// docs`), while a directory target is joined with a slash — the two forms are
// not interchangeable to rclone.
func cloudTargetFor(cloudTarget, bucket string) string {
	sub := cloudObjectPrefix + "/" + bucket
	if strings.HasSuffix(cloudTarget, ":") {
		return cloudTarget + sub
	}
	return cloudTargetPath(cloudTarget, sub)
}

// validateCloudTarget keeps a bucket's cloud target safe as an argv element and
// as a remote path. Two forms are accepted, and nothing else:
//
//   - `remote:` or `remote:path` — a remote in the shared rclone catalog;
//   - `/absolute/directory` — a local directory, which is how tiering is
//     exercised against the pool itself without a cloud account.
//
// A bare name with no colon is rejected rather than being read as a relative
// directory: `b2-archive` typo'd from `b2-archive:` would otherwise store a
// whole bucket in a stray folder next to the daemon and call it "the cloud".
func validateCloudTarget(target string) error {
	if target == "" {
		return fmt.Errorf("cloud_target is required for CLOUD and TIERED buckets")
	}
	if len(target) > 512 {
		return fmt.Errorf("cloud_target is too long")
	}
	if strings.ContainsAny(target, "\n\r\x00") || strings.HasPrefix(target, "-") {
		return fmt.Errorf("cloud_target contains characters that cannot be used")
	}
	if strings.Count(target, ":") > 1 {
		return fmt.Errorf("cloud_target may contain at most one ':' (remote:path)")
	}
	if !strings.Contains(target, ":") {
		if !path.IsAbs(target) {
			return fmt.Errorf("cloud_target %q must be a remote (`name:` or `name:path`) or an absolute directory", target)
		}
		return nil
	}
	name, sub, _ := strings.Cut(target, ":")
	if name == "" {
		return fmt.Errorf("cloud_target must name a remote before the ':'")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("cloud_target remote name %q must not contain a path separator", name)
	}
	if strings.Contains(sub, "::") || strings.Contains(sub, ":") {
		return fmt.Errorf("cloud_target has an empty path segment after the remote name")
	}
	return nil
}

// cloudRemoteName extracts the remote part of a target, or "" for a local
// directory target.
func cloudRemoteName(target string) string {
	if i := strings.Index(target, ":"); i >= 0 {
		return target[:i]
	}
	return ""
}

// cloudTargetPath joins a target and a subpath, keeping a remote's `name:` and
// its path in the one string rclone expects.
func cloudTargetPath(target, sub string) string {
	if sub == "" {
		return target
	}
	return strings.TrimSuffix(target, "/") + "/" + strings.TrimPrefix(sub, "/")
}

// cloudPath is the object's path inside a bucket's cloud target.
func cloudPath(key string) string {
	return path.Clean("/" + key)[1:]
}
