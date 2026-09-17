package main

import (
	"archive/zip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type fileSizeResponse struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Items     int    `json:"items"`
}

func (s *server) handleFileSize(w http.ResponseWriter, r *http.Request) {
	path, rel, err := resolveFilePath(s.filesRoot, r.URL.Query().Get("path"))
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	var size int64
	var items int
	err = filepath.Walk(path, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if current == path {
				return walkErr
			}
			return filepath.SkipDir
		}
		if !info.IsDir() {
			size += info.Size()
			items++
		}
		return nil
	})
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: "calculate size: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, fileSizeResponse{Path: rel, SizeBytes: size, Items: items})
}

func (s *server) handleFileArchive(w http.ResponseWriter, r *http.Request) {
	path, _, err := resolveFilePath(s.filesRoot, r.URL.Query().Get("path"))
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "archive source must be a directory"})
		return
	}
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		name = "onyx-storage"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`.zip"`)
	zw := zip.NewWriter(w)
	walkErr := filepath.Walk(path, func(current string, entry os.FileInfo, walkErr error) error {
		if walkErr != nil {
			if current == path {
				return walkErr
			}
			return filepath.SkipDir
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		relEntry, err := filepath.Rel(path, current)
		if err != nil || relEntry == "." {
			return err
		}
		relEntry = filepath.ToSlash(relEntry)
		if entry.IsDir() {
			_, err = zw.Create(relEntry + "/")
			return err
		}
		file, err := os.Open(current)
		if err != nil {
			return nil
		}
		header, err := zip.FileInfoHeader(entry)
		if err != nil {
			file.Close()
			return err
		}
		header.Name = relEntry
		writer, err := zw.CreateHeader(header)
		if err == nil {
			_, err = io.Copy(writer, file)
		}
		file.Close()
		return err
	})
	closeErr := zw.Close()
	if walkErr != nil || closeErr != nil {
		return
	}
}
