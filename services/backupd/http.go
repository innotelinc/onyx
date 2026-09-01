package main

import (
	"encoding/json"
	"net/http"
)

// newHTTPHandler returns the JSON API surface for backup.onyx.innotel.us
// (docs/design/11 §1.1): healthz + a read-only view over jobs and runs.
// Write operations go through the gRPC contract (and later the onyx-api
// gateway); this surface exists so the subdomain has a real endpoint today.
func newHTTPHandler(s *server) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "onyx-backupd", "version": version})
	})

	mux.HandleFunc("GET /api/v1/backups", func(w http.ResponseWriter, r *http.Request) {
		resp, err := s.ListBackups(r.Context(), nil)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, resp.Runs)
	})

	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		resp, err := s.ListBackupJobs(r.Context(), nil)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, resp.Jobs)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
