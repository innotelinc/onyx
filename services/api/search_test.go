package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// searchFixture builds a small tree with a mix of names, sizes and ages, so a
// filter can be shown to exclude the files it should.
func searchFixture(t *testing.T) *server {
	t.Helper()
	root := t.TempDir()
	mk := func(rel string, size int) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	mk("holiday.mp4", 2048)
	mk("notes/todo.txt", 10)
	mk("notes/holiday-list.txt", 20)
	mk("media/movies/holiday-2026.mkv", 4096)

	old := time.Now().Add(-90 * 24 * time.Hour)
	for _, rel := range []string{"notes/todo.txt", "notes/holiday-list.txt"} {
		if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(rel)), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return &server{filesRoot: root}
}

func search(t *testing.T, s *server, query string) searchResult {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleSearch(w, httptest.NewRequest(http.MethodGet, "/api/v1/files/search"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	var got searchResult
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	return got
}

func names(result searchResult) []string {
	out := make([]string, 0, len(result.Results))
	for _, entry := range result.Results {
		out = append(out, entry.Path)
	}
	return out
}

// A search descends: the whole point over the per-directory filter is finding a
// name that is not in the folder the operator is standing in.
func TestSearchWalksTheTree(t *testing.T) {
	s := searchFixture(t)
	got := search(t, s, "?q=holiday")
	if len(got.Results) != 3 {
		t.Fatalf("results = %v, want the three holiday files", names(got))
	}
	for _, entry := range got.Results {
		if !strings.Contains(entry.Name, "holiday") {
			t.Errorf("result %q does not match the query", entry.Path)
		}
	}
	// Newest first: the two old notes sort below the fresh video files.
	if !strings.Contains(got.Results[0].Path, "mkv") && !strings.Contains(got.Results[0].Path, "mp4") {
		t.Errorf("results are not newest-first: %v", names(got))
	}
	if got.Truncated {
		t.Error("a small tree should not report truncation")
	}
}

// The attribute filters are what make it advanced: size and age narrow a name
// match the Files page cannot express at all.
func TestSearchFiltersBySizeAgeAndType(t *testing.T) {
	s := searchFixture(t)

	large := search(t, s, "?q=holiday&min_size=3000")
	if got := names(large); len(got) != 1 || !strings.Contains(got[0], "mkv") {
		t.Fatalf("min_size results = %v, want only the mkv", got)
	}

	small := search(t, s, "?q=holiday&max_size=2048")
	if len(small.Results) != 2 {
		t.Errorf("max_size results = %v, want the two smaller files", names(small))
	}

	recent := search(t, s, "?q=holiday&modified_since=7d")
	if len(recent.Results) != 2 {
		t.Errorf("modified_since=7d results = %v, want the two fresh files", names(recent))
	}

	dirs := search(t, s, "?q=holiday&type=directory")
	if len(dirs.Results) != 0 {
		t.Errorf("type=directory returned files: %v", names(dirs))
	}

	files := search(t, s, "?q=holiday&type=file")
	if len(files.Results) != 3 {
		t.Errorf("type=file results = %v, want all three", names(files))
	}

	// The root can be narrowed, so a search is scoped to a subtree.
	scoped := search(t, s, "?q=holiday&path=notes")
	if len(scoped.Results) != 1 || !strings.Contains(scoped.Results[0].Path, "notes/holiday-list.txt") {
		t.Errorf("scoped results = %v", names(scoped))
	}
}

// A bounded scan says it was bounded rather than implying it saw everything.
func TestSearchReportsTruncation(t *testing.T) {
	s := searchFixture(t)
	got := search(t, s, "?q=holiday&limit=1")
	if len(got.Results) != 1 {
		t.Fatalf("results = %v, want one", names(got))
	}
	if !got.Truncated {
		t.Error("a limited search must report that more may exist")
	}
	// The default limit is the cap the console can rely on.
	if got.Query != "holiday" {
		t.Errorf("query echoed as %q", got.Query)
	}
}

// Bad input is refused before the tree is walked.
func TestSearchRejectsBadRequests(t *testing.T) {
	s := searchFixture(t)
	for _, target := range []string{
		"/api/v1/files/search",                           // no q
		"/api/v1/files/search?q=",                        // empty q
		"/api/v1/files/search?q=x&type=socket",           // unknown kind
		"/api/v1/files/search?q=x&min_size=-1",           // negative size
		"/api/v1/files/search?q=x&min_size=9&max_size=1", // inverted range
		"/api/v1/files/search?q=x&modified_since=whenever",
		"/api/v1/files/search?q=x&limit=0",
		"/api/v1/files/search?q=x&path=/etc", // absolute path
	} {
		w := httptest.NewRecorder()
		s.handleSearch(w, httptest.NewRequest(http.MethodGet, target, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", target, w.Code, w.Body.String())
		}
	}
}
