package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFilePathConfinesStorageRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, rel, err := resolveFilePath(root, "docs"); err != nil || rel != "docs" {
		t.Fatalf("resolve docs = %q, %v", rel, err)
	}
	if _, _, err := resolveFilePath(root, "../"); err == nil {
		t.Fatal("expected traversal rejection")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, _, err := resolveFilePath(root, "escape"); err == nil {
		t.Fatal("expected symlink escape rejection")
	}
}

func TestHandleFilesListsAndPages(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "a.txt"), []byte("a"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "b.txt"), []byte("bb"), 0o640); err != nil {
		t.Fatal(err)
	}
	s := &server{filesRoot: root}
	for _, tc := range []struct {
		query, wantPath, wantCursor string
		wantEntries                 int
	}{
		{query: "?path=docs&limit=1", wantPath: "docs", wantEntries: 1, wantCursor: "1"},
		{query: "?path=docs&limit=5&q=b", wantPath: "docs", wantEntries: 1},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/files"+tc.query, nil)
		w := httptest.NewRecorder()
		s.handleFiles(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		var got fileListing
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Path != tc.wantPath || len(got.Entries) != tc.wantEntries || got.NextCursor != tc.wantCursor {
			t.Fatalf("listing = %+v", got)
		}
	}
}

func TestHandleFilesRejectsAbsolutePath(t *testing.T) {
	s := &server{filesRoot: t.TempDir()}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/files?path=/etc", nil)
	w := httptest.NewRecorder()
	s.handleFiles(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", w.Code)
	}
}
