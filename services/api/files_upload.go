package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxImportFileBytes int64 = 20 << 30

func (s *server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > maxImportFileBytes && r.ContentLength >= 0 {
		writeEnvelope(w, http.StatusRequestEntityTooLarge, apiError{Code: "too_large", Message: "file exceeds the 20 GB import limit"})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "multipart upload required: " + err.Error()})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "file field is required"})
		return
	}
	defer file.Close()
	parent := r.URL.Query().Get("path")
	name := filepath.Base(filepath.Clean(header.Filename))
	if name == "." || name == ".." || name == "" || strings.Contains(name, string(filepath.Separator)) {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid filename"})
		return
	}
	rel := joinFilePath(parent, name)
	destination, _, err := s.resolveNewFilePath(rel)
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	if _, err := os.Lstat(destination); err == nil {
		writeEnvelope(w, http.StatusConflict, apiError{Code: "already_exists", Message: "file already exists"})
		return
	} else if !os.IsNotExist(err) {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: err.Error()})
		return
	}
	tmp := destination + ".onyx-uploading"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: "create upload: " + err.Error()})
		return
	}
	written, copyErr := io.Copy(out, io.LimitReader(file, maxImportFileBytes+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || written > maxImportFileBytes {
		_ = os.Remove(tmp)
		message := "write upload"
		if written > maxImportFileBytes {
			message = "file exceeds the 20 GB import limit"
		}
		writeEnvelope(w, http.StatusRequestEntityTooLarge, apiError{Code: "too_large", Message: fmt.Sprintf("%s", message)})
		return
	}
	if err := os.Rename(tmp, destination); err != nil {
		_ = os.Remove(tmp)
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: "finalize upload: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, fileMutationResponse{Path: rel})
}
