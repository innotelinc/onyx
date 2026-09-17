package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHandleFileMetaAndContentRange(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.txt"), []byte("0123456789"), 0o640); err != nil {
		t.Fatal(err)
	}
	s := &server{filesRoot: root}

	metaReq := httptest.NewRequest(http.MethodGet, "/api/v1/files/meta?path=sample.txt", nil)
	metaResp := httptest.NewRecorder()
	s.handleFileMeta(metaResp, metaReq)
	if metaResp.Code != http.StatusOK {
		t.Fatalf("metadata status = %d", metaResp.Code)
	}
	var meta fileEntry
	if err := json.Unmarshal(metaResp.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Name != "sample.txt" || meta.Size != 10 || meta.Type != "file" {
		t.Fatalf("metadata = %+v", meta)
	}

	contentReq := httptest.NewRequest(http.MethodGet, "/api/v1/files/content?path=sample.txt", nil)
	contentReq.Header.Set("Range", "bytes=2-5")
	contentResp := httptest.NewRecorder()
	s.handleFileContent(contentResp, contentReq)
	if contentResp.Code != http.StatusPartialContent || contentResp.Body.String() != "2345" {
		t.Fatalf("content = %d %q", contentResp.Code, contentResp.Body.String())
	}
}

func TestHandleFileContentRejectsDirectoryAndTraversal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o750); err != nil {
		t.Fatal(err)
	}
	s := &server{filesRoot: root}
	for path, want := range map[string]int{"folder": http.StatusBadRequest, "../etc/passwd": http.StatusBadRequest, "/etc/passwd": http.StatusBadRequest} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/files/content?path="+path, nil)
		w := httptest.NewRecorder()
		s.handleFileContent(w, r)
		if w.Code != want {
			t.Fatalf("path %q status = %d, want %d", path, w.Code, want)
		}
	}
}
