package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

func TestParseBucketTier(t *testing.T) {
	cases := map[string]onyxv1.BucketTier{
		"":        onyxv1.BucketTier_LOCAL,
		"local":   onyxv1.BucketTier_LOCAL,
		"LOCAL":   onyxv1.BucketTier_LOCAL,
		" cloud ": onyxv1.BucketTier_CLOUD,
		"tiered":  onyxv1.BucketTier_TIERED,
	}
	for in, want := range cases {
		got, ok := parseBucketTier(in)
		if !ok || got != want {
			t.Errorf("parseBucketTier(%q) = (%v, %v), want (%v, true)", in, got, ok, want)
		}
	}
	for _, in := range []string{"bogus", "aws", "BUCKET_TIER_UNSPECIFIED"} {
		if got, ok := parseBucketTier(in); ok {
			t.Errorf("parseBucketTier(%q) = (%v, true), want rejected", in, got)
		}
	}
}

// Every tier the contract defines, except the zero value, must be reachable
// from the gateway: a tier added to objectstore.proto without a name here would
// otherwise be unusable through the API.
func TestParseBucketTierCoversContract(t *testing.T) {
	for name := range onyxv1.BucketTier_value {
		if name == "BUCKET_TIER_UNSPECIFIED" {
			continue
		}
		got, ok := parseBucketTier(strings.ToLower(name))
		if !ok {
			t.Errorf("tier %q in the contract has no gateway mapping", name)
			continue
		}
		if got.String() != name {
			t.Errorf("tier %q maps to %v", name, got)
		}
	}
}

func TestQueryBool(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if v, err := queryBool(r, "force", true); err != nil || !v {
		t.Errorf("absent param: (%v, %v), want (true, nil)", v, err)
	}
	r = httptest.NewRequest(http.MethodGet, "/x?force=false", nil)
	if v, err := queryBool(r, "force", true); err != nil || v {
		t.Errorf("force=false: (%v, %v), want (false, nil)", v, err)
	}
	// A typo must not be interpreted as a boolean default.
	r = httptest.NewRequest(http.MethodGet, "/x?force=yes", nil)
	if _, err := queryBool(r, "force", false); err == nil {
		t.Error("force=yes: expected an error, got none")
	}
}

// The validation paths of the platform routes run before any gRPC call, so a
// zero-value server exercises them without a fake client.

func post(t *testing.T, h http.HandlerFunc, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader(body)))
	return rec
}

func TestCreateBucketRequiresTargetForCloudTier(t *testing.T) {
	s := &server{}
	if rec := post(t, s.handleCreateBucket, "/api/v1/buckets", `{"name":"b","tier":"cloud"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("cloud tier without target: status = %d, want 400", rec.Code)
	}
	if rec := post(t, s.handleCreateBucket, "/api/v1/buckets", `{"name":"b","tier":"bogus","cloud_target":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown tier: status = %d, want 400", rec.Code)
	}
	if rec := post(t, s.handleCreateBucket, "/api/v1/buckets", `{"tier":"local"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing name: status = %d, want 400", rec.Code)
	}
	if rec := post(t, s.handleCreateBucket, "/api/v1/buckets", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want 400", rec.Code)
	}
}

func TestCreateVMRequiresName(t *testing.T) {
	s := &server{}
	if rec := post(t, s.handleCreateVM, "/api/v1/vms", `{"vcpus":2,"memory_mb":1024}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing name: status = %d, want 400", rec.Code)
	}
	if rec := post(t, s.handleCreateVM, "/api/v1/vms", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want 400", rec.Code)
	}
}

func TestInstallAppRequiresAppID(t *testing.T) {
	s := &server{}
	if rec := post(t, s.handleInstallApp, "/api/v1/apps", `{"app_id":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("blank app_id: status = %d, want 400", rec.Code)
	}
}

func TestDeleteVMRejectsUnparsableFlag(t *testing.T) {
	s := &server{}
	rec := httptest.NewRecorder()
	s.handleDeleteVM(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/vms/vm-1?delete_disk=yes", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("delete_disk=yes: status = %d, want 400", rec.Code)
	}
}
