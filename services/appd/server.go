package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Appd (proto/onyx/v1/appd.proto). The catalog
// ships with a small curated sample at v0.1; the signed store + compose
// runtime land with v0.4 (docs/design/09, docs/design/11 §6.4).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedAppdServer

	mu         sync.Mutex
	apps       map[string]*onyxv1.App
	containers map[string]*onyxv1.Container
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.AppdServer = (*server)(nil)

func newServer() *server {
	s := &server{apps: map[string]*onyxv1.App{}, containers: map[string]*onyxv1.Container{}}
	// Curated sample catalog (signed manifests land with the store, v0.4).
	for _, a := range []struct{ id, name, version, desc, manifest string }{
		{"jellyfin", "Jellyfin", "10.9", "Media server", "services:\n  jellyfin:\n    image: jellyfin/jellyfin:latest\n    ports: [\"8096:8096\"]"},
		{"nextcloud", "Nextcloud", "29", "File sync & share", "services:\n  nextcloud:\n    image: nextcloud:apache\n    ports: [\"8080:80\"]"},
		{"photoprism", "PhotoPrism", "240717", "AI photo manager", "services:\n  photoprism:\n    image: photoprism/photoprism:latest\n    ports: [\"2342:2342\"]"},
	} {
		s.apps[a.id] = &onyxv1.App{
			Id:          a.id,
			Name:        a.name,
			Version:     a.version,
			Description: a.desc,
			Manifest:    a.manifest,
			Status:      "not_installed",
		}
	}
	return s
}

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

func (s *server) ListApps(_ context.Context, _ *onyxv1.ListAppsRequest) (*onyxv1.ListAppsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*onyxv1.App, 0, len(s.apps))
	for _, a := range s.apps {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &onyxv1.ListAppsResponse{Apps: out}, nil
}

func (s *server) InstallApp(_ context.Context, req *onyxv1.InstallAppRequest) (*onyxv1.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[req.GetAppId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "app not found")
	}
	if app.Status == "installed" {
		return nil, status.Error(codes.FailedPrecondition, "app already installed")
	}
	app.Status = "installed"
	app.InstalledAt = time.Now().UTC().Format(time.RFC3339)
	// One container per installed app at v0.1; the manifest-driven compose
	// expansion lands with v0.4.
	container := &onyxv1.Container{
		Id:     fmt.Sprintf("c-%d", time.Now().UnixNano()),
		Name:   "app-" + app.Id,
		Image:  app.Id + ":latest",
		Status: "created",
		AppId:  app.Id,
	}
	s.containers[container.Id] = container
	return app, nil
}

func (s *server) UninstallApp(_ context.Context, req *onyxv1.UninstallAppRequest) (*onyxv1.UninstallAppResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	app, ok := s.apps[req.GetAppId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "app not found")
	}
	if app.Status != "installed" {
		return nil, status.Error(codes.FailedPrecondition, "app is not installed")
	}
	app.Status = "not_installed"
	app.InstalledAt = ""
	for id, c := range s.containers {
		if c.AppId == app.Id {
			if c.Status == "running" {
				return nil, status.Error(codes.FailedPrecondition, "stop the app's containers before uninstalling")
			}
			delete(s.containers, id)
		}
	}
	return &onyxv1.UninstallAppResponse{Uninstalled: true}, nil
}

func (s *server) ListContainers(_ context.Context, req *onyxv1.ListContainersRequest) (*onyxv1.ListContainersResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*onyxv1.Container{}
	for _, c := range s.containers {
		if req.GetAppId() != "" && c.AppId != req.GetAppId() {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &onyxv1.ListContainersResponse{Containers: out}, nil
}

func (s *server) transition(id, want string) (*onyxv1.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.containers[id]
	if !ok {
		return nil, status.Error(codes.NotFound, "container not found")
	}
	c.Status = want
	return c, nil
}

func (s *server) StartContainer(ctx context.Context, req *onyxv1.StartContainerRequest) (*onyxv1.Container, error) {
	return s.transition(req.GetId(), "running")
}

func (s *server) StopContainer(ctx context.Context, req *onyxv1.StopContainerRequest) (*onyxv1.Container, error) {
	return s.transition(req.GetId(), "exited")
}

func (s *server) RestartContainer(ctx context.Context, req *onyxv1.RestartContainerRequest) (*onyxv1.Container, error) {
	return s.transition(req.GetId(), "running")
}
