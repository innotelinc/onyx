package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func trustServer(dt *deviceTrustConfig) *server {
	return &server{deviceTrust: dt, version: version}
}

func getTrust(t *testing.T, s *server, rawQuery string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/devices/trust"+rawQuery, nil)
	w := httptest.NewRecorder()
	s.handleDevicesTrust(w, r)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return w, body
}

func TestDevicesTrustOff(t *testing.T) {
	w, body := getTrust(t, trustServer(loadDeviceTrustConfig()), "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body["enabled"] != false || body["mode"] != "off" {
		t.Fatalf("expected disabled off-mode, got %v", body)
	}
	if _, ok := body["devices"]; !ok {
		t.Fatalf("devices key missing (dashboard contract)")
	}
}

func TestDevicesTrustLocalNotesIssuance(t *testing.T) {
	w, body := getTrust(t, trustServer(&deviceTrustConfig{mode: "local"}), "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body["enabled"] != true || body["mode"] != "local" {
		t.Fatalf("expected enabled local mode, got %v", body)
	}
	note, _ := body["note"].(string)
	if note == "" {
		t.Fatalf("local mode must explain where issuance lives")
	}
}

func TestDevicesTrustCeruleanNotConfigured(t *testing.T) {
	w, _ := getTrust(t, trustServer(&deviceTrustConfig{mode: "cerulean"}), "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestDevicesTrustCeruleanProxies(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"devices": []map[string]any{
				{"name": "laptop", "common_name": "laptop", "status": "active", "extra": "dropped"},
				{"name": "phone", "status": "revoked"},
			},
		})
	}))
	defer upstream.Close()

	dt := &deviceTrustConfig{mode: "cerulean", apiURL: upstream.URL, token: "tok", fleetID: "fleet-9"}
	w, body := getTrust(t, trustServer(dt), "?status=revoked")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("upstream auth header missing, got %q", gotAuth)
	}
	if gotQuery != "status=revoked" {
		t.Fatalf("query not forwarded, got %q", gotQuery)
	}
	if gotPath != "/api/v1/fleet/fleet-9/devices" {
		t.Fatalf("unexpected upstream path %q", gotPath)
	}
	if body["fleet_id"] != "fleet-9" {
		t.Fatalf("fleet_id missing: %v", body)
	}
	raw, _ := json.Marshal(body["devices"])
	var devices []ceruleanDevice
	if err := json.Unmarshal(raw, &devices); err != nil {
		t.Fatalf("devices not projected: %v", err)
	}
	if len(devices) != 2 || devices[0].Name != "laptop" || devices[1].Status != "revoked" {
		t.Fatalf("unexpected devices: %s", raw)
	}
	if devices[0].CommonName == "" {
		t.Fatalf("known fields must be kept")
	}
	if _, body2 := getTrust(t, trustServer(dt), ""); body2["count"].(float64) != 2 {
		t.Fatalf("count missing or wrong: %v", body2["count"])
	}
}

func TestDevicesTrustCeruleanUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusForbidden)
	}))
	defer upstream.Close()

	dt := &deviceTrustConfig{mode: "cerulean", apiURL: upstream.URL, token: "tok", fleetID: "f"}
	w, body := getTrust(t, trustServer(dt), "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "device_trust_upstream" {
		t.Fatalf("expected device_trust_upstream code, got %v", errMap)
	}
	if errMap["retryable"] != true {
		t.Fatalf("upstream errors should be retryable")
	}
}

func TestDevicesTrustCeruleanUnreachable(t *testing.T) {
	// Closed port: connection refused inside the 10s client timeout.
	dt := &deviceTrustConfig{mode: "cerulean", apiURL: "http://127.0.0.1:1", token: "tok", fleetID: "f"}
	_, body := getTrust(t, trustServer(dt), "")
	errMap, _ := body["error"].(map[string]any)
	if errMap["code"] != "device_trust_upstream" {
		t.Fatalf("expected device_trust_upstream code, got %v", errMap)
	}
}
