package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type filePathBody struct {
	Path string `json:"path"`
}
type fileRenameBody struct {
	Path    string `json:"path"`
	NewPath string `json:"new_path"`
}

type fileMutationResponse struct {
	Path      string `json:"path"`
	TrashPath string `json:"trash_path,omitempty"`
}

func (s *server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	var body filePathBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	path, rel, err := s.resolveNewFilePath(body.Path)
	if err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if _, err := os.Lstat(path); err == nil {
		writeEnvelope(w, 409, apiError{Code: "already_exists", Message: "path already exists"})
		return
	} else if !os.IsNotExist(err) {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	if err := os.Mkdir(path, 0o750); err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: "create directory: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, fileMutationResponse{Path: rel})
}

func (s *server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	var body fileRenameBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	source, sourceRel, err := resolveFilePath(s.filesRoot, body.Path)
	if err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if sourceRel == "" {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "storage root cannot be renamed"})
		return
	}
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "source not found"})
		return
	}
	destination, destinationRel, err := s.resolveNewFilePath(body.NewPath)
	if err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if _, err := os.Lstat(destination); err == nil {
		writeEnvelope(w, 409, apiError{Code: "already_exists", Message: "destination already exists"})
		return
	} else if !os.IsNotExist(err) {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	if err := os.Rename(source, destination); err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: "rename: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, fileMutationResponse{Path: destinationRel})
}

func (s *server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	var body filePathBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	source, rel, err := resolveFilePath(s.filesRoot, body.Path)
	if err != nil {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if rel == "" || rel == ".trash" || strings.HasPrefix(rel, ".trash/") {
		writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "trash and storage root cannot be deleted"})
		return
	}
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		writeEnvelope(w, 404, apiError{Code: "not_found", Message: "path not found"})
		return
	}
	// Keep trash on the same mounted filesystem as the source. A shared
	// storage-root trash directory may be unwritable when the root is a
	// container mount, and cross-filesystem renames cannot be atomic.
	trashDir := filepath.Join(filepath.Dir(source), ".trash")
	if err := os.MkdirAll(trashDir, 0o750); err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: err.Error()})
		return
	}
	trashName := fmt.Sprintf("%d-%s", time.Now().UTC().UnixNano(), filepath.Base(source))
	trashPath := filepath.Join(trashDir, trashName)
	if err := os.Rename(source, trashPath); err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: "move to trash: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, fileMutationResponse{Path: rel, TrashPath: ".trash/" + trashName})
}

func (s *server) resolveNewFilePath(requested string) (string, string, error) {
	if requested == "" || filepath.IsAbs(requested) || filepath.Clean(requested) == "." || strings.HasPrefix(filepath.ToSlash(filepath.Clean(requested)), "../") {
		return "", "", fmt.Errorf("path must be a non-root relative path")
	}
	requested = filepath.ToSlash(filepath.Clean(requested))
	parent := filepath.ToSlash(filepath.Dir(requested))
	name := filepath.Base(requested)
	if name == "." || name == ".." || strings.Contains(name, string(filepath.Separator)) {
		return "", "", fmt.Errorf("invalid path")
	}
	parentPath, parentRel, err := resolveFilePath(s.filesRoot, parent)
	if err != nil {
		return "", "", err
	}
	if info, err := os.Stat(parentPath); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("parent directory not found")
	}
	return filepath.Join(parentPath, name), joinFilePath(parentRel, name), nil
}
