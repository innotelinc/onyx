package main

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Advanced search (docs/design/10 Quartz, "advanced search"). The Files page
// already filters one directory by name (`?q=` on GET /api/v1/files), which
// answers "where is this file" only if the operator is already standing in the
// right folder. This walks the tree under a path and matches on name **and**
// attributes — kind, size, age — so a question like "the videos over a gigabyte
// from last month" is one request instead of a manual descent.
//
// The walk is bounded twice over: a cap on entries inspected and a cap on
// results. A search over a real pool is a scan, and a scan that cannot stop is
// worse than one that says it stopped; the response carries `truncated` so the
// console can say so rather than implying it saw everything.

const (
	defaultSearchResults = 100
	maxSearchResults     = 500
	// maxSearchScanned bounds how much of the tree one request will touch, so a
	// search over a cold, enormous pool returns a bounded answer instead of
	// holding a worker until it finishes.
	defaultSearchScanned = 200_000
)

type searchResult struct {
	Query     string      `json:"query"`
	Root      string      `json:"root"`
	Scanned   int         `json:"scanned"`
	Truncated bool        `json:"truncated"`
	Results   []fileEntry `json:"results"`
}

// handleSearch serves GET /api/v1/files/search (the `/files/search?q=` row of
// docs/design/06).
//
//	?q=            name substring, case-insensitive (required)
//	&path=         start directory, relative to the storage root (default: root)
//	&type=         file | directory (default: both)
//	&min_size=     bytes, inclusive
//	&max_size=     bytes, inclusive
//	&modified_since= RFC3339, or a duration like 7d / 12h ("last week")
//	&limit=        max results (default 100, cap 500)
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if query == "" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "q is required"})
		return
	}

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

	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if kind != "" && kind != "file" && kind != "directory" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "type must be file or directory"})
		return
	}

	minSize, err := optionalInt64(r.URL.Query().Get("min_size"))
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "min_size must be a non-negative number of bytes"})
		return
	}
	maxSize, err := optionalInt64(r.URL.Query().Get("max_size"))
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "max_size must be a non-negative number of bytes"})
		return
	}
	if minSize > 0 && maxSize > 0 && minSize > maxSize {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "min_size is greater than max_size"})
		return
	}

	since, err := parseSince(r.URL.Query().Get("modified_since"))
	if err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}

	limit := defaultSearchResults
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n < 1 {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "limit must be a positive integer"})
			return
		}
		if n > maxSearchResults {
			n = maxSearchResults
		}
		limit = n
	}

	out := searchResult{Query: query, Root: rel, Results: []fileEntry{}}

	walkErr := filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory is skipped rather than aborting the
			// whole search: one permission-denied folder must not hide every
			// result behind it.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if current == path {
			return nil
		}
		out.Scanned++
		if out.Scanned > defaultSearchScanned {
			out.Truncated = true
			return fs.SkipAll
		}

		// A symlink is not an entry the operator can act on, and following one
		// could leave the storage root entirely.
		entryInfo, infoErr := entry.Info()
		if infoErr != nil || entryInfo.Mode()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		isDir := entryInfo.IsDir()
		if isDir && staleMountDir(current, entry) {
			return fs.SkipDir
		}
		if kind == "file" && isDir {
			return nil
		}
		if kind == "directory" && !isDir {
			return nil
		}
		if !strings.Contains(strings.ToLower(entry.Name()), query) {
			return nil
		}
		// A directory's own size is a filesystem artifact, so the size filters
		// only apply to regular files; asking for size alone never returns a
		// directory whose inode happens to fall in the range.
		if !isDir {
			size := entryInfo.Size()
			if minSize > 0 && size < minSize {
				return nil
			}
			if maxSize > 0 && size > maxSize {
				return nil
			}
		}
		if !since.IsZero() && entryInfo.ModTime().Before(since) {
			return nil
		}

		if len(out.Results) >= limit {
			out.Truncated = true
			return fs.SkipAll
		}
		relPath, relErr := filepath.Rel(path, current)
		if relErr != nil {
			return nil
		}
		typ := "file"
		if isDir {
			typ = "directory"
		}
		out.Results = append(out.Results, fileEntry{
			Name:       entry.Name(),
			Path:       joinFilePath(rel, filepath.ToSlash(relPath)),
			Type:       typ,
			Size:       entryInfo.Size(),
			ModifiedAt: entryInfo.ModTime().UTC().Format(time.RFC3339),
			Mode:       entryInfo.Mode().Perm().String(),
		})
		return nil
	})
	if walkErr != nil {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: "search: " + walkErr.Error()})
		return
	}

	// Newest first: a search is usually "the recent one", and the walk order is
	// filesystem-dependent, so sorting here keeps answers stable between runs.
	sort.SliceStable(out.Results, func(i, j int) bool {
		return out.Results[i].ModifiedAt > out.Results[j].ModifiedAt
	})
	writeJSON(w, http.StatusOK, out)
}

// optionalInt64 parses a non-negative byte count, treating empty as "unset".
func optionalInt64(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}

// parseSince accepts an absolute time or a look-back duration — "7d", "48h",
// "30m" — because an operator asks "the last week", not a timestamp. Empty
// means no lower bound.
func parseSince(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(raw, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil || days < 0 {
			return time.Time{}, &parseSinceError{}
		}
		return time.Now().Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	if dur, err := time.ParseDuration(raw); err == nil {
		return time.Now().Add(-dur), nil
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, nil
	}
	return time.Time{}, &parseSinceError{}
}

type parseSinceError struct{}

func (*parseSinceError) Error() string {
	return "modified_since must be an RFC3339 timestamp or a look-back like 7d, 12h or 30m"
}
