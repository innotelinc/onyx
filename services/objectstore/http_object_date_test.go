package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

func objectServer(t *testing.T, bucket string) *server {
	t.Helper()
	return &server{
		objects: t.TempDir(),
		buckets: map[string]*onyxv1.Bucket{bucket: {Name: bucket}},
	}
}

func objectRequest(t *testing.T, s *server, method, bucket, key string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.s3Object(rec, httptest.NewRequest(method, "http://storage/"+bucket+"/"+key, nil), bucket, key)
	return rec
}

// TestObjectResponsesCarryLastModified pins the bug that made a backup mirror
// fail on individual objects.
//
// The HEAD response carried Last-Modified and the GET response did not, so a
// client that copies an object — HEAD to stat it, GET to read it — parsed an
// empty date off the GET and aborted the transfer ("Last-Modified time format is
// invalid, failed with unable to parse"). Every S3 client is entitled to read
// that header off a GET, and the two responses must agree, so both are checked
// here rather than only the one that was broken.
func TestObjectResponsesCarryLastModified(t *testing.T) {
	const bucket = "documents"
	const key = "org-1/documents/a.pdf"
	s := objectServer(t, bucket)
	writeObject(t, s.objects, bucket, key, "content")

	path := filepath.Join(s.objects, bucket, filepath.FromSlash(key))
	stamp := time.Date(2026, 9, 16, 1, 33, 7, 0, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatalf("setting the object's mtime: %v", err)
	}

	var dates []string
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		rec := objectRequest(t, s, method, bucket, key)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", method, rec.Code)
		}
		value := rec.Header().Get("Last-Modified")
		if value == "" {
			t.Fatalf("the %s response carries no Last-Modified header", method)
		}
		// The format S3 clients parse. http.TimeFormat is RFC1123 in GMT, which
		// is what minio-go tries against this header.
		parsed, err := time.Parse(http.TimeFormat, value)
		if err != nil {
			t.Fatalf("the %s response's Last-Modified %q is not an HTTP date: %v", method, value, err)
		}
		if !parsed.Equal(stamp) {
			t.Errorf("the %s response says %s, want %s", method, parsed, stamp)
		}
		dates = append(dates, value)
	}
	if dates[0] != dates[1] {
		t.Errorf("HEAD says %q and GET says %q — a client that stats then reads sees two different objects", dates[0], dates[1])
	}
}

func TestObjectGetMissingKeyHasNoDate(t *testing.T) {
	const bucket = "documents"
	s := objectServer(t, bucket)
	rec := objectRequest(t, s, http.MethodGet, bucket, "org-1/documents/absent.pdf")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if value := rec.Header().Get("Last-Modified"); value != "" {
		t.Errorf("a 404 carries Last-Modified %q; there is no object to date", value)
	}
}
