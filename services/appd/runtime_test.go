package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// recordingRunner captures the argv of every engine call, so the exact compose
// invocation is pinned by tests rather than by a live daemon.
type recordingRunner struct {
	calls [][]string
	out   string
	err   error
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.out, r.err
}

func (r *recordingRunner) last() []string {
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

func TestRenderManifestSubstitutesConfigAndHost(t *testing.T) {
	manifest := "ports:\n  - \"{{http_port}}:8096\"\nurl: http://{{host}}/\n"
	got := renderManifest(manifest, map[string]string{"http_port": "9000"}, "onyx.test")
	if !strings.Contains(got, `"9000:8096"`) {
		t.Errorf("port not substituted:\n%s", got)
	}
	if !strings.Contains(got, "http://onyx.test/") {
		t.Errorf("host not substituted:\n%s", got)
	}
	// An unknown placeholder stays visible: a manifest typo must not silently
	// become an empty value the engine accepts.
	if out := renderManifest(manifest, nil, ""); !strings.Contains(out, "{{http_port}}") {
		t.Errorf("unknown placeholder was dropped:\n%s", out)
	}
}

func TestWriteProjectRejectsInjectionAndEmptyManifest(t *testing.T) {
	rt := newComposeRuntime("docker", t.TempDir())
	if _, err := rt.writeProject("app", "   \n", nil); err == nil {
		t.Error("empty manifest: expected an error, got none")
	}
	// A newline in a config value could inject additional YAML keys.
	if _, err := rt.writeProject("app", "services: {}", map[string]string{"path": "/mnt/onyx\n  evil: true"}); err == nil {
		t.Error("newline in a config value: expected an error, got none")
	}
	path, err := rt.writeProject("app", "services: {}", map[string]string{})
	if err != nil {
		t.Fatalf("writeProject: %v", err)
	}
	if filepath.Base(path) != "docker-compose.yml" {
		t.Errorf("path = %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "services: {}" {
		t.Errorf("compose file = %q, %v", body, err)
	}
}

// Every engine call is namespaced to the app's own project: a stray name can
// never collide with the platform stack, and `down` can never touch onyx.
func TestComposeArgvIsNamespacedAndScoped(t *testing.T) {
	r := &recordingRunner{out: "[]"}
	rt := newComposeRuntime("docker", t.TempDir())
	rt.run = r.run
	ctx := context.Background()

	if _, err := rt.Up(ctx, "jellyfin", "services: {}", nil); err != nil {
		t.Fatalf("up: %v", err)
	}
	first := r.calls[0]
	want := []string{"docker", "compose", "-p", "onyx-app-jellyfin", "-f", filepath.Join(rt.root, "jellyfin", "docker-compose.yml"), "up", "-d", "--remove-orphans"}
	if strings.Join(first, " ") != strings.Join(want, " ") {
		t.Errorf("up argv = %v, want %v", first, want)
	}

	if err := rt.Down(ctx, "jellyfin", true); err != nil {
		t.Fatalf("down: %v", err)
	}
	last := r.last()
	if !contains(last, "down") || !contains(last, "--volumes") || !contains(last, "onyx-app-jellyfin") {
		t.Errorf("down argv = %v, want a project-scoped purge", last)
	}

	if err := rt.Verb(ctx, "jellyfin", "jellyfin", "restart"); err != nil {
		t.Fatalf("verb: %v", err)
	}
	last = r.last()
	if !contains(last, "restart") || !contains(last, "jellyfin") || !contains(last, "-p") {
		t.Errorf("verb argv = %v, want a service-scoped restart", last)
	}
	if err := rt.Verb(ctx, "jellyfin", "", "start"); err != nil {
		t.Fatalf("project verb: %v", err)
	}
	if err := rt.Verb(ctx, "jellyfin", "jellyfin", "explode"); err == nil {
		t.Error("unsupported verb: expected an error, got none")
	}
}

// Down on an app that was never materialized is a no-op, not a failure: an
// install that failed before the engine ran still has to be cleanable.
func TestDownWithoutProjectIsANoOp(t *testing.T) {
	r := &recordingRunner{}
	rt := newComposeRuntime("docker", t.TempDir())
	rt.run = r.run
	if err := rt.Down(context.Background(), "never-installed", false); err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("engine calls = %v, want none", r.calls)
	}
}

func TestEngineErrorCarriesStderr(t *testing.T) {
	r := &recordingRunner{err: errors.New("docker compose up -d: permission denied")}
	rt := newComposeRuntime("docker", t.TempDir())
	rt.run = r.run
	_, err := rt.Up(context.Background(), "jellyfin", "services: {}", nil)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the engine's message", err)
	}
}

func TestParseComposePS(t *testing.T) {
	ndjson := `{"ID":"abc","Name":"onyx-jellyfin","Service":"jellyfin","Image":"jellyfin/jellyfin:10.9","State":"running","Status":"Up 3 minutes","Health":""}
{"ID":"def","Name":"onyx-nextcloud-db","Service":"db","Image":"mariadb:11","State":"exited","Status":"Exited (0) 2 minutes ago"}`
	got, err := parseComposePS(ndjson, "nextcloud")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("containers = %d, want 2", len(got))
	}
	if got[0].GetStatus() != "running" || got[0].GetService() != "jellyfin" || got[0].GetAppId() != "nextcloud" {
		t.Errorf("container[0] = %+v", got[0])
	}
	if got[1].GetStatus() != "exited" {
		t.Errorf("container[1] = %+v, want exited", got[1])
	}

	// Compose also emits a JSON array on some versions.
	asArray, err := parseComposePS(`[{"ID":"x","Name":"n","Service":"s","State":"paused"}]`, "app")
	if err != nil || len(asArray) != 1 || asArray[0].GetStatus() != "paused" {
		t.Fatalf("array form: %v %+v", err, asArray)
	}

	empty, err := parseComposePS("  \n", "app")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty output: %v %+v", err, empty)
	}
	if _, err := parseComposePS("{not json}", "app"); err == nil {
		t.Error("malformed json: expected an error, got none")
	}
}

func TestContainerStatusMapping(t *testing.T) {
	cases := []struct {
		row  composePSLine
		want string
	}{
		{composePSLine{State: "running"}, "running"},
		{composePSLine{State: "running", Health: "unhealthy"}, "error"},
		{composePSLine{State: "paused"}, "paused"},
		{composePSLine{State: "created"}, "created"},
		{composePSLine{State: "exited", Status: "Exited (137) 1 minute ago"}, "exited"},
		{composePSLine{State: "restarting"}, "running"},
		{composePSLine{State: "dead"}, "exited"},
		{composePSLine{State: "weird"}, "error"},
		{composePSLine{}, "unknown"},
	}
	for _, c := range cases {
		if got := containerStatus(c.row); got != c.want {
			t.Errorf("containerStatus(%+v) = %q, want %q", c.row, got, c.want)
		}
	}
}

// Status on an app with no compose file reports no containers instead of asking
// the engine about a project that does not exist.
func TestStatusWithoutProject(t *testing.T) {
	r := &recordingRunner{}
	rt := newComposeRuntime("docker", t.TempDir())
	rt.run = r.run
	got, err := rt.Status(context.Background(), "ghost")
	if err != nil || len(got) != 0 {
		t.Fatalf("status = %+v, %v", got, err)
	}
	if len(r.calls) != 0 {
		t.Errorf("engine calls = %v, want none", r.calls)
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

var _ = onyxv1.Container{} // the contract type is part of this file's surface
