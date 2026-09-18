package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// server implements Health, Core, and CoreShares (proto/onyx/v1).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedCoreServer
	onyxv1.UnimplementedCoreSharesServer

	db             *sql.DB
	storaged       onyxv1.StoragedClient
	storagedHealth onyxv1.HealthClient
	sharedHealth   onyxv1.HealthClient
	privdHealth    onyxv1.HealthClient
	snapdHealth    onyxv1.HealthClient
	backupdHealth  onyxv1.HealthClient
	vmmHealth      onyxv1.HealthClient
	appdHealth     onyxv1.HealthClient
	aiHealth       onyxv1.HealthClient
	objstoreHealth onyxv1.HealthClient
	config         *configApplier
}

// serviceHealth pairs a health client with the service name it reports as, in
// the order SystemStatus lists them (docs/design/04#1-service-inventory).
func (s *server) serviceHealth() []struct {
	name   string
	client onyxv1.HealthClient
} {
	return []struct {
		name   string
		client onyxv1.HealthClient
	}{
		{"onyx-storaged", s.storagedHealth},
		{"onyx-shared", s.sharedHealth},
		{"onyx-privd", s.privdHealth},
		{"onyx-snapd", s.snapdHealth},
		{"onyx-backupd", s.backupdHealth},
		{"onyx-vmm", s.vmmHealth},
		{"onyx-appd", s.appdHealth},
		{"onyx-ai", s.aiHealth},
		{"onyx-objectstore", s.objstoreHealth},
	}
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.CoreServer = (*server)(nil)
var _ onyxv1.CoreSharesServer = (*server)(nil)

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

// SystemStatus aggregates core itself plus every registered service, queried
// via each service's Health RPC (docs/design/04#8-observability). Every service
// in the compose/systemd set is listed, so `onyx status` reports the whole
// platform rather than just the storage path.
func (s *server) SystemStatus(ctx context.Context, _ *onyxv1.SystemStatusRequest) (*onyxv1.SystemStatusResponse, error) {
	services := []*onyxv1.ServiceStatus{
		{Name: "onyx-core", Version: version, Status: onyxv1.HealthCheckResponse_SERVING},
	}
	// One shared deadline for the whole fan-out rather than a per-service
	// timeout: status must stay fast even when several daemons are down.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, h := range s.serviceHealth() {
		if h.client == nil {
			continue // not configured in this deployment
		}
		if st := healthOf(ctx, h.client, h.name, 2*time.Second); st != nil {
			services = append(services, st)
		}
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

// CreatePool forwards the explicitly confirmed destructive storage operation
// to onyx-storaged, which enforces removable whole-disk policy.
func (s *server) CreatePool(ctx context.Context, req *onyxv1.CreatePoolRequest) (*onyxv1.Pool, error) {
	return s.storaged.CreatePool(ctx, req)
}
