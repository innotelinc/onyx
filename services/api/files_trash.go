package main

import (
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

type trashStatus struct {
	SizeBytes    int64  `json:"size_bytes"`
	Items        int    `json:"items"`
	StorageTotal int64  `json:"storage_total_bytes"`
	StorageFree  int64  `json:"storage_free_bytes"`
}

func (s *server) handleTrash(w http.ResponseWriter, r *http.Request) {
	status, err := s.trashStatus()
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: err.Error()})
		return
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
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.filesRoot, &fs); err == nil {
		out.StorageTotal = int64(fs.Blocks) * int64(fs.Bsize)
		out.StorageFree = int64(fs.Bavail) * int64(fs.Bsize)
	}
	return out, nil
}

func (s *server) findTrashDirs() ([]string, error) {
	var dirs []string
	err := filepath.Walk(s.filesRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".trash" {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}
		return nil
	})
	return dirs, err
}
