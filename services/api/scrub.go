package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type scrubSchedule struct {
	Pool      string `json:"pool"`
	Enabled   bool   `json:"enabled"`
	Frequency string `json:"frequency"`
	Day       string `json:"day,omitempty"`
	Hour      int    `json:"hour"`
	LastRun   string `json:"last_run,omitempty"`
	NextRun   string `json:"next_run,omitempty"`
	Status    string `json:"status"`
}

type scrubStore struct {
	mu        sync.RWMutex
	path      string
	schedules map[string]*scrubSchedule
}

func newScrubStore(stateDir string) (*scrubStore, error) {
	s := &scrubStore{path: filepath.Join(stateDir, "scrub-schedules.json"), schedules: map[string]*scrubSchedule{}}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.schedules); err != nil {
		return nil, fmt.Errorf("decode scrub schedules: %w", err)
	}
	return s, nil
}
func (s *scrubStore) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.schedules, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *server) handleScrubSchedule(w http.ResponseWriter, r *http.Request) {
	s.scrub.mu.RLock()
	v, ok := s.scrub.schedules[r.PathValue("name")]
	s.scrub.mu.RUnlock()
	if !ok {
		writeJSON(w, 200, scrubSchedule{Pool: r.PathValue("name"), Status: "not-configured"})
		return
	}
	writeJSON(w, 200, v)
}
func (s *server) handleSetScrubSchedule(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled   bool   `json:"enabled"`
		Frequency string `json:"frequency"`
		Day       string `json:"day"`
		Hour      int    `json:"hour"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if in.Frequency != "weekly" && in.Frequency != "monthly" {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "frequency must be weekly or monthly"})
		return
	}
	if in.Hour < 0 || in.Hour > 23 {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "hour must be between 0 and 23"})
		return
	}
	pool := r.PathValue("name")
	now := time.Now().UTC()
	next := now.Add(7 * 24 * time.Hour)
	if in.Frequency == "monthly" {
		next = now.Add(30 * 24 * time.Hour)
	}
	v := &scrubSchedule{Pool: pool, Enabled: in.Enabled, Frequency: in.Frequency, Day: in.Day, Hour: in.Hour, NextRun: next.Format(time.RFC3339), Status: "scheduled"}
	s.scrub.mu.Lock()
	s.scrub.schedules[pool] = v
	err := s.scrub.persistLocked()
	s.scrub.mu.Unlock()
	if err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	writeJSON(w, 200, v)
}
func (s *server) handleRunScrub(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("name")
	s.scrub.mu.Lock()
	v, ok := s.scrub.schedules[pool]
	if !ok {
		v = &scrubSchedule{Pool: pool, Frequency: "weekly"}
		s.scrub.schedules[pool] = v
	}
	v.LastRun = time.Now().UTC().Format(time.RFC3339)
	v.Status = "queued"
	err := s.scrub.persistLocked()
	s.scrub.mu.Unlock()
	if err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"pool": pool, "status": "queued", "message": "scrub queued for the storage data plane"})
}
