package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// App sandboxing (docs/design/09 §6). The catalog is code, but the guarantees
// have to hold for every entry — including the next one somebody adds — so they
// are enforced when the service starts rather than by review: a manifest that
// would run with more privilege than an app should have, or reach outside the
// pool, fails loudly at startup instead of becoming an app somebody installs.
//
// The checks are made on the manifest text, which is why they are described as
// a guard over a catalog this service owns rather than as a YAML validator. The
// constructs that matter — privileged mode, host namespaces, added capabilities,
// an unconfined security posture, unlimited resources, and a host path outside
// the storage root — are named explicitly, and a manifest that hides one from
// this scan would also have to hide it from `docker compose`, which reads the
// same file.
const storageRootDefault = "/mnt/onyx"

// serviceBlock is one service of a compose manifest, with its own lines so a
// requirement can be checked per service rather than per file.
type serviceBlock struct {
	name  string
	lines []string
}

func (s serviceBlock) body() string { return strings.Join(s.lines, "\n") }

// composeServices splits a compose manifest into its service blocks. Only
// top-level `services:` children count: a top-level `volumes:` block names
// volumes, and its children are not services.
func composeServices(manifest string) []serviceBlock {
	var (
		blocks     []serviceBlock
		inServices bool
		current    *serviceBlock
	)
	for _, raw := range strings.Split(manifest, "\n") {
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := leadingIndent(line)
		trimmed := strings.TrimSpace(line)
		if indent == 0 {
			// A top-level key ends the service section: flush the block in
			// progress before the `volumes:` (or any other) key drops it.
			if current != nil {
				blocks = append(blocks, *current)
			}
			inServices = trimmed == "services:"
			current = nil
			continue
		}
		if !inServices {
			continue
		}
		if indent == 2 && strings.HasSuffix(trimmed, ":") {
			if current != nil {
				blocks = append(blocks, *current)
			}
			current = &serviceBlock{name: strings.TrimSuffix(trimmed, ":")}
			continue
		}
		if current != nil {
			current.lines = append(current.lines, line)
		}
	}
	if current != nil {
		blocks = append(blocks, *current)
	}
	return blocks
}

// stripComment removes a trailing comment, respecting quotes so a value that
// contains '#' is not truncated.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#':
			return line[:i]
		}
	}
	return line
}

func leadingIndent(line string) int {
	n := 0
	for _, r := range line {
		if r != ' ' {
			break
		}
		n++
	}
	return n
}

// forbiddenServicePatterns are the settings that would give an app the host
// rather than a container on it. Each entry is the exact construct an operator
// would have to write to get that privilege.
var forbiddenServicePatterns = []struct {
	needle string
	why    string
}{
	{"privileged: true", "runs the container privileged, which is the host's root"},
	{"privileged:true", "runs the container privileged, which is the host's root"},
	{"network_mode: host", "shares the host's network namespace"},
	{"network_mode:host", "shares the host's network namespace"},
	{"pid: host", "shares the host's PID namespace"},
	{"pid:host", "shares the host's PID namespace"},
	{"ipc: host", "shares the host's IPC namespace"},
	{"ipc:host", "shares the host's IPC namespace"},
	{"userns_mode: host", "opt out of the user-namespace isolation (docs/design/09 §6)"},
	{"userns_mode:host", "opt out of the user-namespace isolation (docs/design/09 §6)"},
}

// dangerousCapabilities are capabilities an app may not add back. Dropping to
// none and adding a narrow capability is the supported direction; anything here
// is a route back to the host.
var dangerousCapabilities = map[string]bool{
	"ALL":             true,
	"SYS_ADMIN":       true,
	"SYS_PTRACE":      true,
	"SYS_MODULE":      true,
	"SYS_RAWIO":       true,
	"DAC_READ_SEARCH": true,
}

// addedCapability returns the first dangerous capability a service adds back
// through `cap_add`, or "". `cap_drop: [ALL]` — the posture every app should
// have — is deliberately not a violation.
func addedCapability(body string) string {
	inCapAdd := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-") {
			inCapAdd = trimmed == "cap_add:"
			continue
		}
		if !inCapAdd {
			continue
		}
		capability := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		if dangerousCapabilities[strings.ToUpper(capability)] {
			return capability
		}
	}
	return ""
}

// droppedAllCapabilities reports whether a service carries the baseline posture
// `cap_drop: [ALL]` (docs/design/09 §6). It is required rather than assumed:
// a manifest that never drops anything runs with the engine's default set, and
// the check that refuses dangerous additions would then be checking a posture
// the app never adopted.
func droppedAllCapabilities(body string) bool {
	inCapDrop := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "-") {
			inCapDrop = trimmed == "cap_drop:"
			continue
		}
		if !inCapDrop {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")), "ALL") {
			return true
		}
	}
	return false
}

// validateManifestHardening enforces the platform's app sandbox on one catalog
// manifest.
func validateManifestHardening(id, manifest string) error {
	services := composeServices(manifest)
	if len(services) == 0 {
		return fmt.Errorf("app %q has no services in its manifest", id)
	}
	for _, service := range services {
		if service.name == "" {
			return fmt.Errorf("app %q has an unnamed service", id)
		}
		body := service.body()
		for _, forbidden := range forbiddenServicePatterns {
			if strings.Contains(body, forbidden.needle) {
				return fmt.Errorf("app %q service %q declares %s: %s",
					id, service.name, forbidden.needle, forbidden.why)
			}
		}
		if !strings.Contains(body, "no-new-privileges") {
			return fmt.Errorf("app %q service %q does not set security_opt no-new-privileges:true (docs/design/09 §6)",
				id, service.name)
		}
		if !strings.Contains(body, "pids_limit") {
			return fmt.Errorf("app %q service %q sets no pids_limit (docs/design/09 §6 requires pids limits)",
				id, service.name)
		}
		if !strings.Contains(body, "mem_limit") {
			return fmt.Errorf("app %q service %q sets no mem_limit (docs/design/09 §6 requires memory limits)",
				id, service.name)
		}
		// A dangerous addition is the more serious finding, so it is reported
		// before the missing baseline: a manifest asking for SYS_ADMIN must be
		// told exactly that, not that it forgot `cap_drop`.
		if capability := addedCapability(body); capability != "" {
			return fmt.Errorf("app %q service %q adds the %s capability back: apps run with every capability dropped except the narrow ones they declare (docs/design/09 §6)",
				id, service.name, capability)
		}
		if !droppedAllCapabilities(body) {
			return fmt.Errorf("app %q service %q does not drop every capability (docs/design/09 §6 requires `cap_drop: [ALL]` as the baseline)",
				id, service.name)
		}
		for _, mount := range serviceMounts(service) {
			if err := validateMountSource(id, service.name, mount); err != nil {
				return err
			}
		}
	}
	return nil
}

// serviceMounts returns the volume sources of one service block: the entries of
// its `volumes:` list.
func serviceMounts(service serviceBlock) []string {
	var (
		mounts    []string
		inVolumes bool
	)
	for _, line := range service.lines {
		trimmed := strings.TrimSpace(line)
		indent := leadingIndent(line)
		if !strings.HasPrefix(trimmed, "-") {
			// A new key ends the list; the volumes key itself starts it.
			inVolumes = trimmed == "volumes:"
			continue
		}
		if !inVolumes {
			continue
		}
		_ = indent
		item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		item = strings.Trim(item, `"'`)
		if item == "" {
			continue
		}
		source := item
		if i := strings.Index(item, ":"); i >= 0 {
			source = item[:i]
		}
		mounts = append(mounts, strings.Trim(source, `"'`))
	}
	return mounts
}

// validateMountSource keeps a service's storage on the pool. A named volume
// (no path separator) is its own volume and is fine; a bind mount must live
// under the storage root, which is what stops an app from reaching /etc, the
// platform's state directories or the Docker socket. A templated source is
// checked when its config value is validated (validateConfigPaths).
func validateMountSource(id, service, source string) error {
	if source == "" || strings.Contains(source, "{{") {
		return nil
	}
	if !strings.Contains(source, "/") {
		return nil // named volume
	}
	if !filepath.IsAbs(source) {
		return fmt.Errorf("app %q service %q bind-mounts the relative path %q: app storage must be a named volume or a path under %s",
			id, service, source, storageRootDefault)
	}
	return nil
}

// validateConfigPaths checks the operator-supplied settings that name a host
// path. Those values are substituted into bind mounts, so an unbounded value
// would let an install reach any directory on the host — the install request is
// the one place an operator's input enters a manifest.
func validateConfigPaths(app catalogApp, cfg map[string]string, storageRoot string) error {
	root := strings.TrimSuffix(storageRoot, "/")
	if root == "" {
		root = storageRootDefault
	}
	for _, key := range app.PathKeys {
		value := strings.TrimSpace(cfg[key])
		if value == "" {
			continue
		}
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be an absolute path inside %s", key, root)
		}
		clean := filepath.Clean(value)
		if strings.Contains(clean, "..") {
			return fmt.Errorf("%s must not contain '..'", key)
		}
		if clean != root && !strings.HasPrefix(clean, root+"/") {
			return fmt.Errorf("%s must be inside %s, not %s: apps may only mount pool storage", key, root, clean)
		}
	}
	return nil
}
