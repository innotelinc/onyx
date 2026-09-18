package main

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

type trashStatus struct {
	SizeBytes int64 `json:"size_bytes"`
	Items     int   `json:"items"`
	// Deprecated: the storage totals used to describe whatever filesystem the
	// storage root happened to sit on — on a normal install the host's system
	// disk, not the pool. They now report mounted pool capacity only, and
	// GET /api/v1/storage/overview is the endpoint to use: it carries the
	// per-pool detail and the reason when nothing is visible.
	StorageTotal int64 `json:"storage_total_bytes"`
	StorageFree  int64 `json:"storage_free_bytes"`
}

func (s *server) handleTrash(w http.ResponseWriter, r *http.Request) {
	status, err := s.trashStatus()
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: err.Error()})
		return
	}
	// Capacity comes from the pool-aware overview, never from an unqualified
	// statfs of the storage root. A missing data plane leaves the totals at
	// zero rather than substituting the wrong filesystem's numbers.
	if overview, err := s.storageOverview(r.Context()); err == nil {
		status.StorageTotal, status.StorageFree = overview.TotalBytes, overview.FreeBytes
	} else {
		slog.Warn("trash status: storage overview unavailable", "error", err)
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *server) handleEmptyTrash(w http.ResponseWriter, r *http.Request) {
	trashDirs, err := s.findTrashDirs()
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: err.Error()})
		return
	}
	removed := 0
	for _, dir := range trashDirs {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: "read trash: " + readErr.Error()})
			return
		}
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: "empty trash: " + err.Error()})
				return
			}
			removed++
		}
	}
	status, err := s.trashStatus()
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "trash": status})
}

func (s *server) trashStatus() (trashStatus, error) {
	var out trashStatus
	trashDirs, err := s.findTrashDirs()
	if err != nil {
		return out, err
	}
	for _, dir := range trashDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return out, err
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			out.Items++
			if info.IsDir() {
				_ = filepath.Walk(filepath.Join(dir, entry.Name()), func(_ string, nested os.FileInfo, walkErr error) error {
					if walkErr == nil && nested != nil && !nested.IsDir() {
						out.SizeBytes += nested.Size()
					}
					return nil
				})
			} else {
				out.SizeBytes += info.Size()
			}
		}
	}
	return out, nil
}

func (s *server) findTrashDirs() ([]string, error) {
	var dirs []string
	err := filepath.Walk(s.filesRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Protected directories such as lost+found must not prevent trash
			// accounting for the rest of the mounted storage.
			if path == s.filesRoot {
				return err
			}
			return filepath.SkipDir
		}
		if info.IsDir() && info.Name() == ".trash" {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}
		return nil
	})
	return dirs, err
}
