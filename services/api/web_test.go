package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNASConsoleUsesTheSameOriginAsTheAPI(t *testing.T) {
	dir := t.TempDir()
	for file, text := range map[string]string{"index.html": "<h1>ONYX NAS</h1>", "config.js": "window.ONYX_MODE='nas';", "private.txt": "must not be served"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := &server{webRoot: dir}
	s.registerRoutes()
	for _, path := range []string{"/", "/index.html", "/config.js", "/healthz"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			if path == "/index.html" && rec.Code == http.StatusMovedPermanently {
				continue
			}
			t.Errorf("%s: status %d", path, rec.Code)
		}
		if path == "/" && !strings.Contains(rec.Body.String(), "ONYX NAS") {
			t.Fatal("NAS console missing")
		}
	}
	for _, path := range []string{"/private.txt", "/api/v1/nonexistent", "/missing.js"} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("unexpected static route %s: %d", path, rec.Code)
		}
	}
}
