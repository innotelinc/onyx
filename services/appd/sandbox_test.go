package main

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// hardenedManifest is a manifest that satisfies the sandbox posture, so each
// test can introduce exactly one violation and attribute the failure to it.
const hardenedManifest = `services:
  app:
    image: example/app:1
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    mem_limit: 1g
    volumes:
      - app-data:/data
`

func TestValidateManifestHardeningAcceptsTheCatalog(t *testing.T) {
	// The catalog is what actually ships, so it is the case that matters: a
	// guard that rejects the platform's own apps would be useless.
	if err := validateCatalog(catalog()); err != nil {
		t.Fatalf("the shipped catalog violates the sandbox posture: %v", err)
	}
	if err := validateManifestHardening("example", hardenedManifest); err != nil {
		t.Fatalf("hardened manifest rejected: %v", err)
	}
}

func TestValidateManifestHardeningRejectsPrivilegeEscalation(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name: "privileged",
			manifest: `services:
  app:
    image: example/app:1
    privileged: true
    security_opt:
      - no-new-privileges:true
    pids_limit: 256
    mem_limit: 1g
`,
			want: "privileged",
		},
		{
			name: "host network",
			manifest: `services:
  app:
    image: example/app:1
    network_mode: host
    security_opt:
      - no-new-privileges:true
    pids_limit: 256
    mem_limit: 1g
`,
			want: "network namespace",
		},
		{
			name: "host pid namespace",
			manifest: `services:
  app:
    image: example/app:1
    pid: host
    security_opt:
      - no-new-privileges:true
    pids_limit: 256
    mem_limit: 1g
`,
			want: "PID namespace",
		},
		{
			name: "capability added back",
			manifest: `services:
  app:
    image: example/app:1
    cap_add:
      - SYS_ADMIN
    security_opt:
      - no-new-privileges:true
    pids_limit: 256
    mem_limit: 1g
`,
			want: "SYS_ADMIN",
		},
		{
			name: "no-new-privileges missing",
			manifest: `services:
  app:
    image: example/app:1
    cap_drop:
      - ALL
    pids_limit: 256
    mem_limit: 1g
`,
			want: "no-new-privileges",
		},
		{
			name: "no pids limit",
			manifest: `services:
  app:
    image: example/app:1
    security_opt:
      - no-new-privileges:true
    mem_limit: 1g
`,
			want: "pids_limit",
		},
		{
			name: "no memory limit",
			manifest: `services:
  app:
    image: example/app:1
    security_opt:
      - no-new-privileges:true
    pids_limit: 256
`,
			want: "mem_limit",
		},
		{
			name: "relative bind mount",
			manifest: `services:
  app:
    image: example/app:1
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    mem_limit: 1g
    volumes:
      - ./data:/data
`,
			want: "relative path",
		},
		{
			// One hardened service does not cover its neighbour: a manifest is
			// only as safe as its least confined container.
			name: "second service unhardened",
			manifest: hardenedManifest + `  sidecar:
    image: example/sidecar:1
`,
			want: "sidecar",
		},
		{
			name:     "no services",
			manifest: "version: \"3\"\n",
			want:     "no services",
		},
	}
	for _, c := range cases {
		err := validateManifestHardening("example", c.manifest)
		if err == nil {
			t.Errorf("%s: manifest was accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}

// Dropping everything and adding back a narrow capability is the supported
// direction (docs/design/09 §6): an image whose entrypoint chowns its data and
// then drops to its own user needs CHOWN/SETUID/SETGID, and without them the
// shipped Nextcloud database crash-looped with "failed switching to 'mysql'" —
// a working catalog must be able to declare them.
func TestValidateManifestAllowsNarrowCapabilityAdditions(t *testing.T) {
	manifest := `services:
  app:
    image: example/app:1
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    cap_add:
      - CHOWN
      - SETUID
      - SETGID
    pids_limit: 256
    mem_limit: 1g
`
	if err := validateManifestHardening("example", manifest); err != nil {
		t.Fatalf("a declared narrow capability must be accepted: %v", err)
	}
}

// The baseline itself is enforced, not assumed: a manifest that drops nothing
// runs with the engine's default set, and the check that refuses dangerous
// additions would then be policing a posture the app never adopted.
func TestValidateManifestRequiresDroppingEveryCapability(t *testing.T) {
	manifest := `services:
  app:
    image: example/app:1
    security_opt:
      - no-new-privileges:true
    pids_limit: 256
    mem_limit: 1g
`
	err := validateManifestHardening("example", manifest)
	if err == nil || !strings.Contains(err.Error(), "cap_drop") {
		t.Fatalf("expected the cap_drop baseline to be required, got %v", err)
	}
}

// A named volume is the app's own storage and needs no pool path; a bind mount
// on the pool is the supported way to reach data the operator already has.
func TestValidateMountSourceAcceptsNamedVolumesAndPoolPaths(t *testing.T) {
	manifest := `services:
  app:
    image: example/app:1
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    mem_limit: 1g
    volumes:
      - app-data:/data
      - /mnt/onyx/media:/media:ro
      - "{{media_path}}:/media2:ro"
`
	if err := validateManifestHardening("example", manifest); err != nil {
		t.Fatalf("a named volume, a pool path and a templated path should all pass: %v", err)
	}
}

// The install request is the one place operator input reaches a manifest, and
// its path settings become bind mounts — so they are confined to the pool.
func TestValidateConfigPathsConfinesInstallsToThePool(t *testing.T) {
	app := catalogApp{ID: "example", PathKeys: []string{"media_path"}}
	cases := []struct {
		value string
		ok    bool
	}{
		{"/mnt/onyx", true},
		{"/mnt/onyx/photos", true},
		{"/mnt/onyx/media/sub", true},
		{"", true}, // unset: the catalog default applies
		{"/etc", false},
		{"/mnt/onyx-other", false}, // a sibling of the root, not inside it
		{"/mnt/onyx/../etc", false},
		{"relative/path", false},
		{"/var/lib/onyx/appd/apps", false},
	}
	for _, c := range cases {
		err := validateConfigPaths(app, map[string]string{"media_path": c.value}, "/mnt/onyx")
		if c.ok && err != nil {
			t.Errorf("%q: %v, want accepted", c.value, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%q: accepted, want refused", c.value)
		}
	}
}

// The guard has to be on the install path, not only at startup: an operator
// pointing an app at /etc must be refused before anything is written.
func TestInstallRejectsAPathOutsideThePool(t *testing.T) {
	rt := &fakeRuntime{}
	s := testServer(t, rt)

	_, err := s.InstallApp(context.Background(), &onyxv1.InstallAppRequest{
		AppId:  "jellyfin",
		Config: map[string]string{"media_path": "/etc"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	if !strings.Contains(err.Error(), "/mnt/onyx") {
		t.Errorf("error %q should say where apps may mount from", err)
	}
	if rt.upCalls != 0 {
		t.Errorf("the engine was asked to start %d project(s) for a refused install", rt.upCalls)
	}
	// A pool path installs normally.
	if _, err := s.InstallApp(context.Background(), &onyxv1.InstallAppRequest{
		AppId:  "jellyfin",
		Config: map[string]string{"media_path": "/mnt/onyx/media"},
	}); err != nil {
		t.Fatalf("install with a pool path: %v", err)
	}
}
