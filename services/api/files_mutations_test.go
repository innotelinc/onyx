package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func postJSON(t *testing.T, s *server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestFileMutationsUseTrash(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	s := &server{filesRoot: root}
	s.registerRoutes()
	w := postJSON(t, s, "/api/v1/files/rename", `{"path":"old.txt","new_path":"new.txt"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("rename status = %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}

	w = postJSON(t, s, "/api/v1/files/delete", `{"path":"new.txt"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
	}
	var deleted fileMutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.TrashPath == "" {
		t.Fatal("missing trash path")
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(deleted.TrashPath))); err != nil {
		t.Fatalf("trash entry missing: %v", err)
	}
}

func TestFileMkdirRejectsTraversalAndCreatesFolder(t *testing.T) {
	root := t.TempDir()
	s := &server{filesRoot: root}
	s.registerRoutes()
	w := postJSON(t, s, "/api/v1/files/mkdir", `{"path":"folder"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("mkdir status = %d: %s", w.Code, w.Body.String())
	}
	if info, err := os.Stat(filepath.Join(root, "folder")); err != nil || !info.IsDir() {
		t.Fatalf("folder missing: %v", err)
	}
	w = postJSON(t, s, "/api/v1/files/mkdir", `{"path":"../escape"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal status = %d", w.Code)
	}
}
