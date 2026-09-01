package main

import (
	"context"
	"log/slog"
	"time"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Core (proto/onyx/v1).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedCoreServer

	storaged       onyxv1.StoragedClient
	storagedHealth onyxv1.HealthClient
	privdHealth    onyxv1.HealthClient
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.CoreServer = (*server)(nil)

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

// SystemStatus aggregates core itself plus every registered service, queried
// via each service's Health RPC (docs/design/04#8-observability).
func (s *server) SystemStatus(ctx context.Context, _ *onyxv1.SystemStatusRequest) (*onyxv1.SystemStatusResponse, error) {
	services := []*onyxv1.ServiceStatus{
		{Name: "onyx-core", Version: version, Status: onyxv1.HealthCheckResponse_SERVING},
	}
	if st := healthOf(ctx, s.storagedHealth, "onyx-storaged", 2*time.Second); st != nil {
		services = append(services, st)
	}
	if st := healthOf(ctx, s.privdHealth, "onyx-privd", 2*time.Second); st != nil {
		services = append(services, st)
	}
	return &onyxv1.SystemStatusResponse{CoreVersion: version, Services: services}, nil
}

func healthOf(ctx context.Context, c onyxv1.HealthClient, name string, timeout time.Duration) *onyxv1.ServiceStatus {
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.Check(sctx, &onyxv1.HealthCheckRequest{Service: name})
	if err != nil {
		slog.Warn("health check failed", "service", name, "error", err)
		return &onyxv1.ServiceStatus{Name: name, Version: "unknown", Status: onyxv1.HealthCheckResponse_UNKNOWN}
	}
	return &onyxv1.ServiceStatus{Name: name, Version: resp.Version, Status: resp.Status}
}

// ListPools forwards to onyx-storaged: the control plane reaches the filesystem
// only through the data plane's gRPC interface (docs/design/02#1-system-overview).
func (s *server) ListPools(ctx context.Context, req *onyxv1.ListPoolsRequest) (*onyxv1.ListPoolsResponse, error) {
	return s.storaged.ListPools(ctx, req)
}

// GetPool returns one pool by name, forwarded to onyx-storaged. storaged's
// not_found status passes through as a 404 at the gateway.
func (s *server) GetPool(ctx context.Context, req *onyxv1.GetPoolRequest) (*onyxv1.Pool, error) {
	return s.storaged.GetPool(ctx, req)
}