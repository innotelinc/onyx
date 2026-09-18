package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// Runtime is the container engine behind onyx-appd (docs/design/11 §6.4). The
// compose implementation below is the real one; tests inject a fake so the
// server's policy can be checked without a Docker daemon.
type Runtime interface {
	// Up materializes an app's compose project and starts it.
	Up(ctx context.Context, appID, manifest string, config map[string]string) ([]*onyxv1.Container, error)
	// Down stops and removes an app's project. purgeVolumes also removes its
	// named volumes — that is what makes "uninstall and delete data" real.
	Down(ctx context.Context, appID string, purgeVolumes bool) error
	// Verb runs one compose lifecycle verb for a whole project (service == "")
	// or for a single service.
	Verb(ctx context.Context, appID, service, verb string) error
	// Status lists the project's containers from the engine itself.
	Status(ctx context.Context, appID string) ([]*onyxv1.Container, error)
}

// projectPrefix namespaces every appd-managed compose project, so a stray
// project name can never collide with the platform stack (and a `down` can
// never take onyx itself down).
const projectPrefix = "onyx-app-"

// runner runs one command and returns its combined stderr on failure. It is a
// field on composeRuntime so tests can assert the exact argv.
type runner func(ctx context.Context, name string, args ...string) (string, error)

func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return string(out), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), detail)
	}
	return string(out), nil
}

type composeRuntime struct {
	bin string
	// root holds one directory per app: its compose file and any generated
	// assets. The manifest is written here so `docker compose -f` has a file
	// to read, which is what makes the app's definition inspectable by hand.
	root    string
	run     runner
	timeout time.Duration
}

func newComposeRuntime(bin, root string) *composeRuntime {
	return &composeRuntime{bin: bin, root: root, run: execRunner, timeout: 3 * time.Minute}
}

// projectName is the compose project for an app.
func projectName(appID string) string { return projectPrefix + appID }

// composeFile is the app's compose file path.
func (r *composeRuntime) composeFile(appID string) string {
	return filepath.Join(r.root, appID, "docker-compose.yml")
}

// writeProject writes the app's compose file, rendering ${key} placeholders
// from the install-time config. Values are validated for newlines so a config
// value cannot inject additional YAML keys.
func (r *composeRuntime) writeProject(appID, manifest string, config map[string]string) (string, error) {
	if strings.TrimSpace(manifest) == "" {
		return "", errors.New("app manifest is empty")
	}
	for key, value := range config {
		if strings.ContainsAny(key+value, "\r\n") {
			return "", fmt.Errorf("config value for %q must be a single line", key)
		}
		manifest = strings.ReplaceAll(manifest, "${"+key+"}", value)
	}
	dir := filepath.Join(r.root, appID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create app directory: %w", err)
	}
	path := filepath.Join(dir, "docker-compose.yml")
	tmp := path + ".tmp"
	// 0640: the file may carry ports and host paths, but it never holds secrets
	// — those belong to the platform's secret store, not an app manifest.
	if err := os.WriteFile(tmp, []byte(manifest), 0o640); err != nil {
		return "", fmt.Errorf("write compose file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("install compose file: %w", err)
	}
	return path, nil
}

func (r *composeRuntime) Up(ctx context.Context, appID, manifest string, config map[string]string) ([]*onyxv1.Container, error) {
	path, err := r.writeProject(appID, manifest, config)
	if err != nil {
		return nil, err
	}
	// --remove-orphans: a service deleted from the manifest must not linger.
	// -d: unattended apps do not hold the installer hostage.
	if _, err := r.compose(ctx, appID, path, "up", "-d", "--remove-orphans"); err != nil {
		return nil, err
	}
	return r.Status(ctx, appID)
}

func (r *composeRuntime) Down(ctx context.Context, appID string, purgeVolumes bool) error {
	path := r.composeFile(appID)
	if _, err := os.Stat(path); err != nil {
		// Nothing materialized for this app: there is no project to take down.
		// Not an error — an app recorded by an earlier version, or an install
		// that failed before the engine ran, still has to be cleanable.
		return nil
	}
	args := []string{"down", "--remove-orphans"}
	if purgeVolumes {
		args = append(args, "--volumes")
	}
	_, err := r.compose(ctx, appID, path, args...)
	return err
}

func (r *composeRuntime) Verb(ctx context.Context, appID, service, verb string) error {
	switch verb {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("unsupported lifecycle verb %q", verb)
	}
	path := r.composeFile(appID)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("app %s has no compose project (was it installed by this service?)", appID)
	}
	args := []string{"compose", "-p", projectName(appID), "-f", path, verb}
	if service != "" {
		args = append(args, service)
	}
	_, err := r.runTimeout(ctx, args...)
	return err
}

func (r *composeRuntime) Status(ctx context.Context, appID string) ([]*onyxv1.Container, error) {
	path := r.composeFile(appID)
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	out, err := r.compose(ctx, appID, path, "ps", "--format", "json", "--all")
	if err != nil {
		return nil, err
	}
	return parseComposePS(out, appID)
}

func (r *composeRuntime) compose(ctx context.Context, appID, path string, args ...string) (string, error) {
	full := append([]string{"compose", "-p", projectName(appID), "-f", path}, args...)
	return r.runTimeout(ctx, full...)
}

// runTimeout bounds an engine call: a wedged Docker daemon must not hold an
// uninstall (or an install) open forever.
func (r *composeRuntime) runTimeout(ctx context.Context, args ...string) (string, error) {
	if r.timeout <= 0 {
		return r.run(ctx, r.bin, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.run(ctx, r.bin, args...)
}

// composePSLine is the subset of `docker compose ps --format json` this service
// needs. Compose emits one JSON object per line (or a JSON array, depending on
// the version), which parseComposePS handles both of.
type composePSLine struct {
	ID      string `json:"ID"`
	Name    string `json:"Name"`
	Service string `json:"Service"`
	Image   string `json:"Image"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Health  string `json:"Health"`
}

// parseComposePS turns compose's container listing into the contract's
// containers, mapping engine states onto the documented set
// (created | running | paused | exited | error).
func parseComposePS(out, appID string) ([]*onyxv1.Container, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	var rows []composePSLine
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			return nil, fmt.Errorf("parse compose ps: %w", err)
		}
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var row composePSLine
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				return nil, fmt.Errorf("parse compose ps line: %w", err)
			}
			rows = append(rows, row)
		}
	}
	containers := make([]*onyxv1.Container, 0, len(rows))
	for _, row := range rows {
		containers = append(containers, &onyxv1.Container{
			Id:      row.ID,
			Name:    row.Name,
			Image:   row.Image,
			Status:  containerStatus(row),
			AppId:   appID,
			Service: row.Service,
		})
	}
	return containers, nil
}

// containerStatus maps an engine state (plus health, when the image declares a
// healthcheck) onto the contract's vocabulary. A container that is up but
// unhealthy reads as "error": that is the state an operator has to act on.
func containerStatus(row composePSLine) string {
	state := strings.ToLower(strings.TrimSpace(row.State))
	health := strings.ToLower(strings.TrimSpace(row.Health))
	if health == "unhealthy" {
		return "error"
	}
	switch state {
	case "running":
		return "running"
	case "paused":
		return "paused"
	case "created":
		return "created"
	case "exited", "dead", "removing":
		if strings.Contains(strings.ToLower(row.Status), "exit ") {
			// Keep "exited" even for a nonzero exit code: the code is in the
			// engine's own view, and the UI links to logs for the detail.
			return "exited"
		}
		return "exited"
	case "restarting":
		return "running"
	default:
		if state == "" {
			return "unknown"
		}
		return "error"
	}
}
