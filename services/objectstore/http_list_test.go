package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeObject puts bytes at a key inside a bucket directory, creating parents.
func writeObject(t *testing.T, objects, bucket, key, body string) {
	t.Helper()
	path := filepath.Join(objects, bucket, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating object directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing object: %v", err)
	}
}

func listObjects(t *testing.T, objects, bucket, query string) (int, string) {
	t.Helper()
	s := &server{objects: objects}
	rec := httptest.NewRecorder()
	s.s3ListObjects(rec, httptest.NewRequest("GET", "http://storage/"+bucket+query, nil), bucket)
	return rec.Code, rec.Body.String()
}

// TestListObjectsIncludesNestedKeys pins the bug that a listing only ever saw
// the bucket's top level.
//
// Signara writes every document to `<org>/documents/<uuid>.pdf`. A listing that
// skipped directories therefore reported an empty bucket while the objects were
// sitting right there in it — and anything built on that listing (a backup, an
// inventory, an age-out sweep) would have omitted every document without an
// error. It failed silently, which is what makes it worth a test rather than a
// fix-and-forget.
func TestListObjectsIncludesNestedKeys(t *testing.T) {
	objects := t.TempDir()
	const bucket = "documents"
	writeObject(t, objects, bucket, "flat.txt", "flat")
	writeObject(t, objects, bucket, "org-1/documents/a.pdf", "a")
	writeObject(t, objects, bucket, "org-1/documents/b.pdf", "b")
	writeObject(t, objects, bucket, "org-2/documents/c.pdf", "c")

	code, body := listObjects(t, objects, bucket, "")
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", code, body)
	}
	for _, want := range []string{
		"flat.txt",
		"org-1/documents/a.pdf",
		"org-1/documents/b.pdf",
		"org-2/documents/c.pdf",
	} {
		if !strings.Contains(body, "<Key>"+want+"</Key>") {
			t.Errorf("the listing does not contain %q\n%s", want, body)
		}
	}
	if !strings.Contains(body, "<KeyCount>4</KeyCount>") {
		t.Errorf("KeyCount does not count the nested keys\n%s", body)
	}
}

func TestListObjectsHonoursPrefix(t *testing.T) {
	objects := t.TempDir()
	const bucket = "documents"
	writeObject(t, objects, bucket, "org-1/documents/a.pdf", "a")
	writeObject(t, objects, bucket, "org-2/documents/c.pdf", "c")

	_, body := listObjects(t, objects, bucket, "?prefix=org-1/")
	if !strings.Contains(body, "org-1/documents/a.pdf") {
		t.Errorf("a prefix that matches was dropped\n%s", body)
	}
	if strings.Contains(body, "org-2/documents/c.pdf") {
		t.Errorf("a prefix that does not match was included\n%s", body)
	}
}

// TestListObjectsDelimiterRollsUpCommonPrefixes covers the `ls`-style walk every
// S3 client can do: one level at a time, with the children rolled up.
func TestListObjectsDelimiterRollsUpCommonPrefixes(t *testing.T) {
	objects := t.TempDir()
	const bucket = "documents"
	writeObject(t, objects, bucket, "flat.txt", "flat")
	writeObject(t, objects, bucket, "org-1/documents/a.pdf", "a")
	writeObject(t, objects, bucket, "org-2/documents/c.pdf", "c")

	_, body := listObjects(t, objects, bucket, "?delimiter=/")
	if !strings.Contains(body, "<Key>flat.txt</Key>") {
		t.Errorf("a key with no delimiter in it should still be listed\n%s", body)
	}
	for _, want := range []string{"<Prefix>org-1/</Prefix>", "<Prefix>org-2/</Prefix>"} {
		if !strings.Contains(body, want) {
			t.Errorf("CommonPrefixes is missing %s\n%s", want, body)
		}
	}
	if strings.Contains(body, "<Key>org-1/documents/a.pdf</Key>") {
		t.Errorf("keys below the delimiter should be rolled up, not listed\n%s", body)
	}
}

func TestListObjectsMissingBucket(t *testing.T) {
	code, body := listObjects(t, t.TempDir(), "absent", "")
	if code != 404 {
		t.Fatalf("status = %d, want 404", code)
	}
	if !strings.Contains(body, "NoSuchBucket") {
		t.Errorf("expected a NoSuchBucket error, got %s", body)
	}
}
