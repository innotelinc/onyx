package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// fakeRuntime records what the server asked the engine to do, so the policy can
// be checked without a Docker daemon.
type fakeRuntime struct {
	upCalls   int
	lastUp    map[string]string
	lastMani  string
	downCalls []struct {
		app   string
		purge bool
	}
	verbs []struct {
		app, service, verb string
	}
	containers []*onyxv1.Container
	upErr      error
	downErr    error
	verbErr    error
	statusErr  error
}

func (f *fakeRuntime) Up(_ context.Context, appID, manifest string, config map[string]string) ([]*onyxv1.Container, error) {
	f.upCalls++
	f.lastMani = manifest
	f.lastUp = config
	if f.upErr != nil {
		return nil, f.upErr
	}
	return f.containers, nil
}

func (f *fakeRuntime) Down(_ context.Context, appID string, purgeVolumes bool) error {
	f.downCalls = append(f.downCalls, struct {
		app   string
		purge bool
	}{appID, purgeVolumes})
	return f.downErr
}

func (f *fakeRuntime) Verb(_ context.Context, appID, service, verb string) error {
	f.verbs = append(f.verbs, struct {
		app, service, verb string
	}{appID, service, verb})
	return f.verbErr
}

func (f *fakeRuntime) Status(_ context.Context, _ string) ([]*onyxv1.Container, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.containers, nil
}

func testServer(t *testing.T, rt Runtime) *server {
	t.Helper()
	st, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	apps := catalog()
	if err := validateCatalog(apps); err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return newServer(apps, st, rt, "onyx.test", storageRootDefault)
}

func jellyfinContainer() *onyxv1.Container {
	return &onyxv1.Container{
		Id: "c1", Name: "onyx-jellyfin", Image: "jellyfin/jellyfin:10.9",
		Status: "running", AppId: "jellyfin", Service: "jellyfin",
	}
}

func TestInstallPersistsAndStarts(t *testing.T) {
	rt := &fakeRuntime{containers: []*onyxv1.Container{jellyfinContainer()}}
	s := testServer(t, rt)
	ctx := context.Background()

	app, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "jellyfin", Config: map[string]string{"http_port": "9000"}})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if app.GetStatus() != "installed" || app.GetInstalledAt() == "" {
		t.Errorf("app = %+v, want installed with a timestamp", app)
	}
	if got := rt.lastMani; !strings.Contains(got, `"9000:8096"`) {
		t.Errorf("manifest did not take the configured port:\n%s", got)
	}
	// The whole catalog defaults are recorded, not just what the caller sent.
	if rt.lastUp["media_path"] != "/mnt/onyx" || rt.lastUp["http_port"] != "9000" {
		t.Errorf("engine config = %v, want defaults merged with the request", rt.lastUp)
	}

	apps, err := s.ListApps(ctx, &onyxv1.ListAppsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(apps.GetApps()) != len(catalog()) {
		t.Fatalf("apps = %d, want the whole catalog", len(apps.GetApps()))
	}
	if got := appByID(t, apps.GetApps(), "jellyfin"); got.GetStatus() != "installed" || got.GetConfig()["http_port"] != "9000" {
		t.Errorf("jellyfin = %+v, want installed with the recorded config", got)
	}
	if got := appByID(t, apps.GetApps(), "nextcloud"); got.GetStatus() != "not_installed" {
		t.Errorf("nextcloud = %+v, want not_installed", got)
	}

	containers, err := s.ListContainers(ctx, &onyxv1.ListContainersRequest{})
	if err != nil {
		t.Fatalf("containers: %v", err)
	}
	if len(containers.GetContainers()) != 1 || containers.GetContainers()[0].GetService() != "jellyfin" {
		t.Errorf("containers = %+v, want the engine's one container", containers.GetContainers())
	}
}

// The install must survive a restart of the service, not just of the request.
func TestInstallSurvivesRestart(t *testing.T) {
	rt := &fakeRuntime{containers: []*onyxv1.Container{jellyfinContainer()}}
	dir := t.TempDir()
	st, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	apps := catalog()
	s := newServer(apps, st, rt, "", storageRootDefault)
	if _, err := s.InstallApp(context.Background(), &onyxv1.InstallAppRequest{AppId: "jellyfin"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	st.Close()

	st2, err := openStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	restarted := newServer(apps, st2, rt, "", storageRootDefault)
	resp, err := restarted.ListApps(context.Background(), &onyxv1.ListAppsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := appByID(t, resp.GetApps(), "jellyfin"); got.GetStatus() != "installed" {
		t.Errorf("after restart jellyfin = %+v, want installed", got)
	}
}

func TestInstallRejectsBadRequests(t *testing.T) {
	rt := &fakeRuntime{containers: []*onyxv1.Container{jellyfinContainer()}}
	s := testServer(t, rt)
	ctx := context.Background()

	if _, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown app: %v, want NotFound", err)
	}
	if _, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "jellyfin", Version: "1.0"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("pinned version: %v, want FailedPrecondition", err)
	}
	if _, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "jellyfin"}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "jellyfin"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second install: %v, want FailedPrecondition", err)
	}
	if rt.upCalls != 1 {
		t.Errorf("engine up calls = %d, want 1 (rejections must not reach the engine)", rt.upCalls)
	}
}

// A failed install records nothing: the UI must not show a half-installed app.
func TestInstallFailureRecordsNothing(t *testing.T) {
	rt := &fakeRuntime{upErr: errors.New("Cannot connect to the Docker daemon")}
	s := testServer(t, rt)
	ctx := context.Background()

	_, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "jellyfin"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Docker daemon") {
		t.Fatalf("install error = %v, want the engine's message as FailedPrecondition", err)
	}
	resp, err := s.ListApps(ctx, &onyxv1.ListAppsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := appByID(t, resp.GetApps(), "jellyfin"); got.GetStatus() != "not_installed" {
		t.Errorf("jellyfin = %+v, want not_installed after a failed install", got)
	}
	installed, err := s.store.listInstalled()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if len(installed) != 0 {
		t.Errorf("installed = %v, want none", installed)
	}
}

func TestUninstallPolicy(t *testing.T) {
	running := []*onyxv1.Container{jellyfinContainer()}
	rt := &fakeRuntime{containers: running}
	s := testServer(t, rt)
	ctx := context.Background()

	if _, err := s.UninstallApp(ctx, &onyxv1.UninstallAppRequest{AppId: "jellyfin"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("uninstall of an app that is not installed: %v, want FailedPrecondition", err)
	}

	if _, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "jellyfin"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// A running container must not be killed by an uninstall.
	if _, err := s.UninstallApp(ctx, &onyxv1.UninstallAppRequest{AppId: "jellyfin"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("uninstall with a running container: %v, want FailedPrecondition", err)
	}
	if len(rt.downCalls) != 0 {
		t.Errorf("engine down calls = %d, want 0", len(rt.downCalls))
	}

	if _, err := s.UninstallApp(ctx, &onyxv1.UninstallAppRequest{AppId: "jellyfin", Force: true, PurgeData: true}); err != nil {
		t.Fatalf("forced uninstall: %v", err)
	}
	if len(rt.downCalls) != 1 || !rt.downCalls[0].purge || rt.downCalls[0].app != "jellyfin" {
		t.Errorf("down calls = %+v, want one purge of jellyfin", rt.downCalls)
	}
	resp, err := s.ListApps(ctx, &onyxv1.ListAppsRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := appByID(t, resp.GetApps(), "jellyfin"); got.GetStatus() != "not_installed" {
		t.Errorf("jellyfin = %+v, want not_installed after uninstall", got)
	}
}

func TestContainerVerbTargetsTheAppsService(t *testing.T) {
	rt := &fakeRuntime{containers: []*onyxv1.Container{jellyfinContainer()}}
	s := testServer(t, rt)
	ctx := context.Background()
	if _, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "jellyfin"}); err != nil {
		t.Fatalf("install: %v", err)
	}

	if _, err := s.StopContainer(ctx, &onyxv1.StopContainerRequest{Id: "c1"}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(rt.verbs) != 1 || rt.verbs[0] != (struct{ app, service, verb string }{"jellyfin", "jellyfin", "stop"}) {
		t.Fatalf("verbs = %+v, want stop jellyfin/jellyfin", rt.verbs)
	}
	if _, err := s.RestartContainer(ctx, &onyxv1.RestartContainerRequest{Id: "c1"}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if len(rt.verbs) != 2 || rt.verbs[1].verb != "restart" {
		t.Fatalf("verbs = %+v, want a restart too", rt.verbs)
	}
	if _, err := s.StartContainer(ctx, &onyxv1.StartContainerRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown container: %v, want NotFound", err)
	}
	if _, err := s.StartContainer(ctx, &onyxv1.StartContainerRequest{}); status.Code(err) != codes.NotFound {
		t.Errorf("empty container id: %v, want NotFound", err)
	}
}

// With the engine unreachable the recorded state is still served, and a
// completed verb is not reported as a failure just because the re-read failed.
func TestEngineUnavailableFallsBackToRecordedState(t *testing.T) {
	rt := &fakeRuntime{containers: []*onyxv1.Container{jellyfinContainer()}}
	s := testServer(t, rt)
	ctx := context.Background()
	if _, err := s.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: "jellyfin"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	rt.statusErr = errors.New("Cannot connect to the Docker daemon")

	containers, err := s.ListContainers(ctx, &onyxv1.ListContainersRequest{})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers.GetContainers()) != 1 || containers.GetContainers()[0].GetStatus() != "running" {
		t.Errorf("containers = %+v, want the recorded running container", containers.GetContainers())
	}

	got, err := s.StopContainer(ctx, &onyxv1.StopContainerRequest{Id: "c1"})
	if err != nil {
		t.Fatalf("stop with the engine unreachable after the verb: %v", err)
	}
	if got.GetStatus() != "exited" {
		t.Errorf("status = %q, want exited", got.GetStatus())
	}
	recorded, err := s.store.listContainers("jellyfin")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if len(recorded) != 1 || recorded[0].Status != "exited" {
		t.Errorf("recorded = %+v, want the intended state persisted", recorded)
	}
}

func TestListContainersRejectsUnknownAppFilter(t *testing.T) {
	s := testServer(t, &fakeRuntime{})
	_, err := s.ListContainers(context.Background(), &onyxv1.ListContainersRequest{AppId: "ghost"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("filter for an uninstalled app: %v, want NotFound", err)
	}
}

func appByID(t *testing.T, apps []*onyxv1.App, id string) *onyxv1.App {
	t.Helper()
	for _, a := range apps {
		if a.GetId() == id {
			return a
		}
	}
	t.Fatalf("app %s not in %d catalog entries", id, len(apps))
	return nil
}
