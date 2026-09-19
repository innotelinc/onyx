package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// DeletePool is how a stale record is cleared, so the request it issues is the
// contract: the e2e harness and the console both rely on DELETE
// /api/v1/pools/<name> existing and on the name reaching the API verbatim.
func TestDeletePoolIssuesDeleteOnThePoolPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"pool":      map[string]any{"name": "main-pool", "uuid": "uuid-1", "state": "offline"},
			"unmounted": false,
		})
	}))
	defer srv.Close()

	if err := New(srv.URL).DeletePool(t.Context(), "main-pool"); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v1/pools/main-pool" {
		t.Errorf("issued %s %s, want DELETE /api/v1/pools/main-pool", gotMethod, gotPath)
	}
}

// A pool that is not there is reported as the API's not_found, not as success:
// a console that silently "removed" an unknown pool would hide a stale list.
func TestDeletePoolSurfacesNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "not_found", "message": "pool 'gone' not found"},
		})
	}))
	defer srv.Close()

	err := New(srv.URL).DeletePool(t.Context(), "gone")
	if err == nil {
		t.Fatal("expected an error for a missing pool")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Code != "not_found" {
		t.Fatalf("error = %#v, want a not_found API error", err)
	}
}

// The API serializes protobuf messages with protojson, which emits 64-bit
// fields as JSON *strings*. A client that only accepted numbers would fail to
// decode the very responses it asks for, so the wire forms are pinned here.
func TestVMDecodesProtoJSONInt64Strings(t *testing.T) {
	var vm VM
	body := `{"id":"vm-1","name":"debian","status":"stopped","vcpus":2,"memoryMb":"2048","disk":"/mnt/onyx/main-pool/@apps/vms/debian.qcow2","os":"debian-12","createdAt":"2026-09-18T00:00:00Z"}`
	if err := json.Unmarshal([]byte(body), &vm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if vm.MemoryMB != 2048 {
		t.Errorf("memoryMb = %d, want 2048", vm.MemoryMB)
	}
	if vm.VCPUs != 2 || vm.Name != "debian" {
		t.Errorf("vm = %+v", vm)
	}
}

func TestBucketDecodesProtoJSONInt64Strings(t *testing.T) {
	var buckets Buckets
	body := `{"buckets":[{"name":"docs","tier":"TIERED","cloudTarget":"b2-archive:","localObjects":"3","cloudObjects":"3","lastSyncAt":"2026-09-18T00:00:00Z","evictAfterDays":30}]}`
	if err := json.Unmarshal([]byte(body), &buckets); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(buckets.Buckets) != 1 {
		t.Fatalf("buckets = %+v", buckets.Buckets)
	}
	b := buckets.Buckets[0]
	if b.LocalObjects != 3 || b.CloudObjects != 3 || b.EvictAfterDays != 30 {
		t.Errorf("bucket = %+v", b)
	}
}

func TestBucketDecodesMissingCountersAsZero(t *testing.T) {
	var b Bucket
	if err := json.Unmarshal([]byte(`{"name":"docs","tier":"LOCAL"}`), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.LocalObjects != 0 || b.CloudObjects != 0 {
		t.Errorf("absent counters should read as zero, got %+v", b)
	}
}

func TestSyncBucketResultDecodesProtoJSONInt64Strings(t *testing.T) {
	var result SyncBucketResult
	body := `{"bucket":{"name":"docs","tier":"TIERED"},"uploaded":"5","evicted":"2","detail":"transferred 5 file(s)","warnings":["one local copy could not be released"]}`
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.Uploaded != 5 || result.Evicted != 2 {
		t.Errorf("result = %+v", result)
	}
	if len(result.Warnings) != 1 {
		t.Errorf("warnings = %v", result.Warnings)
	}
}

// Outbound requests use the snake_case body the API decodes, not the protojson
// camelCase it returns — a mismatch here would install the wrong app config.
func TestInstallAppRequestSerializesSnakeCase(t *testing.T) {
	raw, err := json.Marshal(&InstallAppRequest{
		AppID:   "nextcloud",
		Version: "29.0.0",
		Config:  map[string]string{"port": "8080"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	for _, want := range []string{`"app_id":"nextcloud"`, `"version":"29.0.0"`, `"config":{"port":"8080"}`} {
		if !contains(got, want) {
			t.Errorf("body %s is missing %s", got, want)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
