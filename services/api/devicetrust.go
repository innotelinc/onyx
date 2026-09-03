package main

// Device trust surface (docs/design/11 §10.3): GET /api/v1/devices/trust
// exposes the enrolled-device fleet to the dashboard. In DEVICE_TRUST=cerulean
// mode it proxies Cerulean's device list — issuance and revocation live in the
// Cerulean dashboard, ONYX only reads. In off/local modes it answers locally
// without any upstream call.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const deviceTrustUpstreamTimeout = 10 * time.Second

// deviceTrustConfig is the env-driven device-trust posture of this gateway.
type deviceTrustConfig struct {
	mode    string // off | local | cerulean
	apiURL  string // CERULEAN_API_URL (cerulean mode)
	token   string // CERULEAN_API_TOKEN
	fleetID string // FLEET_ID
	http    *http.Client
}

func loadDeviceTrustConfig() *deviceTrustConfig {
	mode := strings.ToLower(os.Getenv("DEVICE_TRUST"))
	if mode == "" {
		mode = "off"
	}
	return &deviceTrustConfig{
		mode:    mode,
		apiURL:  strings.TrimRight(os.Getenv("CERULEAN_API_URL"), "/"),
		token:   os.Getenv("CERULEAN_API_TOKEN"),
		fleetID: os.Getenv("FLEET_ID"),
		http:    &http.Client{Timeout: deviceTrustUpstreamTimeout},
	}
}

// ceruleanDevice is the projected form of a Cerulean-enrolled device. Unknown
// upstream fields are dropped; the shape is the dashboard contract.
type ceruleanDevice struct {
	Name       string `json:"name"`
	CommonName string `json:"common_name,omitempty"`
	Status     string `json:"status,omitempty"`
	IssuedAt   string `json:"issued_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

type ceruleanDevicesResponse struct {
	Devices []ceruleanDevice `json:"devices"`
}

// handleDevicesTrust serves GET /api/v1/devices/trust.
//
//	cerulean: proxy of GET {CERULEAN_API_URL}/api/v1/fleet/{FLEET_ID}/devices
//	          (query params are passed through, e.g. ?status=revoked)
//	local:    200 with an issuance note; certificates never came from Cerulean
//	off:      200 with enabled=false
func (s *server) handleDevicesTrust(w http.ResponseWriter, r *http.Request) {
	dt := s.deviceTrust
	if dt == nil {
		dt = &deviceTrustConfig{mode: "off"}
	}
	switch dt.mode {
	case "local":
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":    "local",
			"enabled": true,
			"devices": []ceruleanDevice{},
			"note":    "device certificates are issued locally (scripts/provision-device-trust.sh); no Cerulean fleet is attached",
		})
	case "cerulean":
		if dt.apiURL == "" || dt.token == "" || dt.fleetID == "" {
			writeEnvelope(w, http.StatusServiceUnavailable, apiError{
				Code:      "device_trust_not_configured",
				Message:   "DEVICE_TRUST=cerulean requires CERULEAN_API_URL, CERULEAN_API_TOKEN and FLEET_ID",
				RequestID: rIDFrom(r),
			})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), deviceTrustUpstreamTimeout)
		defer cancel()
		devices, err := dt.fetchDevices(ctx, r.URL.RawQuery)
		if err != nil {
			writeEnvelope(w, http.StatusBadGateway, apiError{
				Code:      "device_trust_upstream",
				Message:   err.Error(),
				RequestID: rIDFrom(r),
				Retryable: true,
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":     "cerulean",
			"enabled":  true,
			"fleet_id": dt.fleetID,
			"devices":  devices,
			"count":    len(devices),
		})
	default: // off
		writeJSON(w, http.StatusOK, map[string]any{
			"mode":    "off",
			"enabled": false,
			"devices": []ceruleanDevice{},
		})
	}
}

// fetchDevices calls Cerulean and projects the fleet's device list.
func (dt *deviceTrustConfig) fetchDevices(ctx context.Context, rawQuery string) ([]ceruleanDevice, error) {
	u := dt.apiURL + "/api/v1/fleet/" + dt.fleetID + "/devices"
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build cerulean request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+dt.token)
	req.Header.Set("Accept", "application/json")

	client := dt.http
	if client == nil { // configs built outside loadDeviceTrustConfig (tests)
		client = &http.Client{Timeout: deviceTrustUpstreamTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cerulean API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("cerulean API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed ceruleanDevicesResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("cerulean API returned invalid JSON: %w", err)
	}
	if parsed.Devices == nil {
		parsed.Devices = []ceruleanDevice{}
	}
	return parsed.Devices, nil
}
