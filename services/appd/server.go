package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Appd (proto/onyx/v1/appd.proto). Apps come from
// the curated catalog, installations and containers are persisted in SQLite,
// and every lifecycle action runs against the compose engine through the
// Runtime seam (docs/design/09, docs/design/11 §6.4).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedAppdServer

	catalog map[string]catalogApp
	store   *store
	runtime Runtime
	host    string
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.AppdServer = (*server)(nil)

func newServer(appCatalog map[string]catalogApp, st *store, rt Runtime, host string) *server {
	return &server{catalog: appCatalog, store: st, runtime: rt, host: host}
}

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

func (s *server) ListApps(_ context.Context, _ *onyxv1.ListAppsRequest) (*onyxv1.ListAppsResponse, error) {
	installed, err := s.store.listInstalled()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	apps := make([]*onyxv1.App, 0, len(s.catalog))
	for _, app := range s.catalog {
		rec, ok := installed[app.ID]
		var ptr *installedApp
		if ok {
			ptr = &rec
		}
		apps = append(apps, catalogAppToProto(app, ptr))
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	return &onyxv1.ListAppsResponse{Apps: apps}, nil
}

// InstallApp materializes the app's compose project and starts it. A failure
// from the engine leaves nothing recorded: the app is not installed, and the
// caller gets the engine's own error text instead of a half-installed app the
// UI would happily show.
func (s *server) InstallApp(ctx context.Context, req *onyxv1.InstallAppRequest) (*onyxv1.App, error) {
	app, ok := s.catalog[req.GetAppId()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "app %q is not in the catalog", req.GetAppId())
	}
	if req.GetVersion() != "" && req.GetVersion() != app.Version {
		return nil, status.Errorf(codes.FailedPrecondition,
			"version %s is not available for %s (the catalog has %s)", req.GetVersion(), app.ID, app.Version)
	}
	installed, err := s.store.listInstalled()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if _, exists := installed[app.ID]; exists {
		return nil, status.Errorf(codes.FailedPrecondition, "app %s is already installed", app.ID)
	}

	cfg := catalogConfig(app, req.GetConfig())
	manifest := renderManifest(app.Manifest, cfg, s.host)
	containers, err := s.runtime.Up(ctx, app.ID, manifest, cfg)
	if err != nil {
		slog.Error("app install failed", "app", app.ID, "error", err)
		return nil, status.Errorf(codes.FailedPrecondition, "install %s: %v", app.ID, err)
	}
	if err := s.store.markInstalled(app.ID, app.Version, cfg); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := s.store.replaceContainers(app.ID, toRecords(containers)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Read the record back rather than inventing one: the install timestamp is
	// the store's, so what the caller gets is exactly what was persisted.
	recorded, err := s.store.listInstalled()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	rec, ok := recorded[app.ID]
	if !ok {
		return nil, status.Errorf(codes.Internal, "app %s was not persisted", app.ID)
	}
	slog.Info("app installed", "app", app.ID, "version", app.Version, "containers", len(containers))
	return catalogAppToProto(app, &rec), nil
}

// UninstallApp stops the app's project and removes it. Running containers are
// refused unless the caller forces it, because "uninstall" must not silently
// kill a service someone is using.
func (s *server) UninstallApp(ctx context.Context, req *onyxv1.UninstallAppRequest) (*onyxv1.UninstallAppResponse, error) {
	appID := req.GetAppId()
	installed, err := s.store.listInstalled()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if _, ok := installed[appID]; !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "app %s is not installed", appID)
	}
	containers, err := s.store.listContainers(appID)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	running := 0
	for _, c := range containers {
		if c.Status == "running" {
			running++
		}
	}
	if running > 0 && !req.GetForce() {
		return nil, status.Errorf(codes.FailedPrecondition,
			"app %s has %d running container(s); stop them first or uninstall with force", appID, running)
	}
	if err := s.runtime.Down(ctx, appID, req.GetPurgeData()); err != nil {
		slog.Error("app uninstall failed", "app", appID, "error", err)
		return nil, status.Errorf(codes.FailedPrecondition, "uninstall %s: %v", appID, err)
	}
	if err := s.store.markUninstalled(appID); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	slog.Info("app uninstalled", "app", appID, "purged_data", req.GetPurgeData())
	return &onyxv1.UninstallAppResponse{Uninstalled: true}, nil
}

// ListContainers reports the engine's live view for every installed app, and
// falls back to the last recorded view when the engine cannot be reached — an
// app whose containers exist is still an app, even while Docker is restarting.
func (s *server) ListContainers(ctx context.Context, req *onyxv1.ListContainersRequest) (*onyxv1.ListContainersResponse, error) {
	installed, err := s.store.listInstalled()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	appIDs := make([]string, 0, len(installed))
	for id := range installed {
		if req.GetAppId() == "" || req.GetAppId() == id {
			appIDs = append(appIDs, id)
		}
	}
	if req.GetAppId() != "" && len(appIDs) == 0 {
		return nil, status.Errorf(codes.NotFound, "app %s is not installed", req.GetAppId())
	}
	sort.Strings(appIDs)

	out := []*onyxv1.Container{}
	for _, id := range appIDs {
		live, err := s.runtime.Status(ctx, id)
		if err != nil {
			slog.Warn("container listing fell back to the recorded state", "app", id, "error", err)
			recorded, recErr := s.store.listContainers(id)
			if recErr != nil {
				return nil, status.Error(codes.Internal, recErr.Error())
			}
			out = append(out, recordsToProto(recorded)...)
			continue
		}
		if err := s.store.replaceContainers(id, toRecords(live)); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		out = append(out, live...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &onyxv1.ListContainersResponse{Containers: out}, nil
}

func (s *server) StartContainer(ctx context.Context, req *onyxv1.StartContainerRequest) (*onyxv1.Container, error) {
	return s.containerVerb(ctx, req.GetId(), "start")
}

func (s *server) StopContainer(ctx context.Context, req *onyxv1.StopContainerRequest) (*onyxv1.Container, error) {
	return s.containerVerb(ctx, req.GetId(), "stop")
}

func (s *server) RestartContainer(ctx context.Context, req *onyxv1.RestartContainerRequest) (*onyxv1.Container, error) {
	return s.containerVerb(ctx, req.GetId(), "restart")
}

// containerVerb runs one compose lifecycle verb for the service a container
// belongs to, then re-reads the engine's view so the returned container is the
// engine's answer rather than an optimistic guess.
func (s *server) containerVerb(ctx context.Context, containerID, verb string) (*onyxv1.Container, error) {
	rec, ok, err := s.store.containerByID(containerID)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "container %q is not tracked by onyx-appd", containerID)
	}
	if err := s.runtime.Verb(ctx, rec.AppID, rec.Service, verb); err != nil {
		slog.Error("container lifecycle failed", "container", containerID, "verb", verb, "error", err)
		return nil, status.Errorf(codes.FailedPrecondition, "%s %s: %v", verb, rec.Name, err)
	}
	containers, err := s.runtime.Status(ctx, rec.AppID)
	if err != nil {
		// The verb succeeded; the engine just cannot be re-read. Record the
		// intended state and say so rather than failing a completed action.
		slog.Warn("container status refresh failed", "app", rec.AppID, "error", err)
		intended := map[string]string{"start": "running", "stop": "exited", "restart": "running"}[verb]
		if err := s.store.setContainerStatus(containerID, intended); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		rec.Status = intended
		return recordToProto(rec), nil
	}
	if err := s.store.replaceContainers(rec.AppID, toRecords(containers)); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	for _, c := range containers {
		if c.GetId() == containerID {
			return c, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "container %q disappeared from app %s", containerID, rec.AppID)
}

// --- helpers ---

func toRecords(containers []*onyxv1.Container) []containerRecord {
	out := make([]containerRecord, 0, len(containers))
	for _, c := range containers {
		out = append(out, containerRecord{
			ID: c.GetId(), AppID: c.GetAppId(), Service: c.GetService(),
			Name: c.GetName(), Image: c.GetImage(), Status: c.GetStatus(),
		})
	}
	return out
}

func recordsToProto(records []containerRecord) []*onyxv1.Container {
	out := make([]*onyxv1.Container, 0, len(records))
	for _, r := range records {
		out = append(out, recordToProto(r))
	}
	return out
}

func recordToProto(r containerRecord) *onyxv1.Container {
	return &onyxv1.Container{
		Id: r.ID, Name: r.Name, Image: r.Image, Status: r.Status, AppId: r.AppID, Service: r.Service,
	}
}

// renderManifest substitutes {{key}} placeholders. Unknown placeholders are
// left intact so a manifest typo is visible in the compose file rather than
// becoming an empty string the engine silently accepts.
func renderManifest(manifest string, cfg map[string]string, host string) string {
	values := make(map[string]string, len(cfg)+1)
	for k, v := range cfg {
		values[k] = v
	}
	if host != "" {
		values["host"] = host
	}
	for key, value := range values {
		manifest = strings.ReplaceAll(manifest, "{{"+key+"}}", value)
	}
	return manifest
}

// validateAppID is used by tests and by any future store integration.
func validateAppID(id string) error {
	if !appIDRe.MatchString(id) {
		return fmt.Errorf("app id %q must match %s", id, appIDRe)
	}
	return nil
}
