package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultFilePageSize = 100
const maxFilePageSize = 500

type fileEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	Mode       string `json:"mode"`
}

type fileListing struct {
	Path       string      `json:"path"`
	Entries    []fileEntry `json:"entries"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func resolveFilePath(root, requested string) (string, string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", fmt.Errorf("storage root: %w", err)
	}
	if requested == "" {
		requested = "."
	}
	if filepath.IsAbs(requested) {
		return "", "", fmt.Errorf("path must be relative to storage root")
	}
	candidate := filepath.Join(rootAbs, filepath.Clean(requested))
	candidateReal, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", "", fmt.Errorf("path: %w", err)
	}
	if candidateReal != rootReal && !strings.HasPrefix(candidateReal, rootReal+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("path escapes storage root")
	}
	rel, err := filepath.Rel(rootReal, candidateReal)
	if err != nil {
		return "", "", err
	}
	if rel == "." {
		rel = ""
	}
	return candidateReal, filepath.ToSlash(rel), nil
}

func (s *server) handleFiles(w http.ResponseWriter, r *http.Request) {
	path, rel, err := resolveFilePath(s.filesRoot, r.URL.Query().Get("path"))
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeEnvelope(w, http.StatusNotFound, apiError{Code: "not_found", Message: "directory not found"})
		return
	}
	if !info.IsDir() {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "path is not a directory"})
		return
	}
	limit := defaultFilePageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 1 {
			writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "limit must be a positive number"})
			return
		}
		if n > maxFilePageSize {
			n = maxFilePageSize
		}
		limit = n
	}
	cursor := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cursor, err = strconv.Atoi(raw)
		if err != nil || cursor < 0 {
			writeEnvelope(w, 400, apiError{Code: "invalid_argument", Message: "cursor must be a non-negative number"})
			return
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		writeEnvelope(w, 500, apiError{Code: "internal", Message: "read directory: " + err.Error()})
		return
	}
	needle := strings.ToLower(r.URL.Query().Get("q"))
	filtered := entries[:0]
	for _, entry := range entries {
		if needle == "" || strings.Contains(strings.ToLower(entry.Name()), needle) {
			filtered = append(filtered, entry)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].IsDir() != filtered[j].IsDir() {
			return filtered[i].IsDir()
		}
		return strings.ToLower(filtered[i].Name()) < strings.ToLower(filtered[j].Name())
	})
	if cursor > len(filtered) {
		cursor = len(filtered)
	}
	end := cursor + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	out := fileListing{Path: rel, Entries: make([]fileEntry, 0, end-cursor)}
	for _, entry := range filtered[cursor:end] {
		entryInfo, infoErr := entry.Info()
		if infoErr != nil || entryInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		typ := "file"
		if entryInfo.IsDir() {
			typ = "directory"
		}
		out.Entries = append(out.Entries, fileEntry{Name: entry.Name(), Path: joinFilePath(rel, entry.Name()), Type: typ, Size: entryInfo.Size(), ModifiedAt: entryInfo.ModTime().UTC().Format(time.RFC3339), Mode: entryInfo.Mode().Perm().String()})
	}
	if end < len(filtered) {
		out.NextCursor = strconv.Itoa(end)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleFileMeta(w http.ResponseWriter, r *http.Request) {
	path, rel, err := resolveFilePath(s.filesRoot, r.URL.Query().Get("path"))
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		writeEnvelope(w, http.StatusNotFound, apiError{Code: "not_found", Message: "file not found"})
		return
	}
	typ := "file"
	if info.IsDir() {
		typ = "directory"
	}
	writeJSON(w, http.StatusOK, fileEntry{Name: info.Name(), Path: rel, Type: typ, Size: info.Size(), ModifiedAt: info.ModTime().UTC().Format(time.RFC3339), Mode: info.Mode().Perm().String()})
}

func (s *server) handleFileContent(w http.ResponseWriter, r *http.Request) {
	path, _, err := resolveFilePath(s.filesRoot, r.URL.Query().Get("path"))
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		writeEnvelope(w, http.StatusNotFound, apiError{Code: "not_found", Message: "file not found"})
		return
	}
	if !info.Mode().IsRegular() {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "content is only available for regular files"})
		return
	}
	w.Header().Set("Content-Disposition", `inline; filename="`+strings.ReplaceAll(info.Name(), `"`, "")+`"`)
	http.ServeFile(w, r, path)
}

func joinFilePath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}
