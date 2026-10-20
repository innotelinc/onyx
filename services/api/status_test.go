package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
	releaseversion "github.com/innotelinc/onyx/services/version"
)

// statusCore is only the SystemStatus call: the status endpoint must not reach
// for anything else, and the embedded nil interface makes that loud if it does.
type statusCore struct {
	onyxv1.CoreClient
	resp *onyxv1.SystemStatusResponse
}

func (s *statusCore) SystemStatus(_ context.Context, _ *onyxv1.SystemStatusRequest, _ ...grpc.CallOption) (*onyxv1.SystemStatusResponse, error) {
	return s.resp, nil
}

// The console prints the release on its overview. coreVersion identifies the
// daemons, but the codename and commit belong to this gateway build, so they
// have to be in the same answer rather than a second request that can disagree.
func TestSystemVersionReportsTheGatewayRelease(t *testing.T) {
	srv := &server{core: &statusCore{resp: &onyxv1.SystemStatusResponse{CoreVersion: "0.3.0-dev"}}}
	rec := httptest.NewRecorder()
	srv.handleVersion(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/version", nil))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["version"] != releaseversion.Version {
		t.Errorf("version = %v, want %s", body["version"], releaseversion.Version)
	}
	if body["core_version"] != "0.3.0-dev" {
		t.Errorf("core_version = %v, want 0.3.0-dev", body["core_version"])
	}
}

func TestSystemStatusCarriesReleaseMetadata(t *testing.T) {
	srv := &server{core: &statusCore{resp: &onyxv1.SystemStatusResponse{CoreVersion: "0.4.0"}}}
	rec := httptest.NewRecorder()
	srv.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["coreVersion"] != "0.4.0" {
		t.Errorf("coreVersion = %v, want 0.4.0", body["coreVersion"])
	}
	if body["version"] != releaseversion.Version {
		t.Errorf("version = %v, want %s", body["version"], releaseversion.Version)
	}
	if body["codename"] != releaseversion.Codename {
		t.Errorf("codename = %v, want %s", body["codename"], releaseversion.Codename)
	}
	if body["commit"] != releaseversion.Commit {
		t.Errorf("commit = %v, want %s", body["commit"], releaseversion.Commit)
	}
}
