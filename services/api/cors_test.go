package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSAllowsDashboardPreflight(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/system/status", nil)
	req.Header.Set("Origin", "https://app.onyx.innotel.us")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	resp := httptest.NewRecorder()

	withCORS(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight should not reach the API handler")
	})).ServeHTTP(resp, req)

	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusNoContent)
	}
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "https://app.onyx.innotel.us" {
		t.Fatalf("allow-origin = %q", got)
	}
}

func TestCORSDoesNotReflectUnknownOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp := httptest.NewRecorder()

	withCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(resp, req)

	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unexpected allow-origin = %q", got)
	}
}
