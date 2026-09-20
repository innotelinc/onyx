package main

import (
	"net/http"
	"testing"
)

// TestPrefixKeyIsNotAnObject pins the bug that let a backup mirror prune nothing
// while reporting success.
//
// Objects are files under the bucket directory, so the key `postgres` is a
// directory exactly when objects were written beneath `postgres/…` — a prefix,
// which S3 has no object for. The handler stat'ed the path and answered 200 to
// `HEAD <bucket>/postgres` because the directory existed; rclone probes with HEAD
// to decide file-versus-directory, concluded the prefix was a single object, and
// then listed nothing beneath it and refused to apply `--min-age` to a "file" it
// could not age. The mirror grew without bound for weeks while every run logged
// success. It is every S3 client that is affected, not just rclone — a client
// SDK that stats an object before downloading it would have tried to GET it.
//
// HEAD and GET are both checked against the same key on purpose: a client
// normally stats with HEAD and then reads with GET, so answering 404 to one and
// 200 to the other would leave the store disagreeing with itself about whether
// the key exists.
func TestPrefixKeyIsNotAnObject(t *testing.T) {
	const bucket = "backups"
	s := objectServer(t, bucket)
	writeObject(t, s.objects, bucket, "postgres/db-2026-09-20.dump", "dump")

	for _, key := range []string{"postgres", "postgres/"} {
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			rec := objectRequest(t, s, method, bucket, key)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s /%s/%s = %d, want 404: a key that names a prefix is not an object (body: %s)",
					method, bucket, key, rec.Code, rec.Body.String())
			}
		}
	}

	// The object under the prefix is still an object: the fix must not swallow
	// the keys the prefix stands for.
	if rec := objectRequest(t, s, http.MethodHead, bucket, "postgres/db-2026-09-20.dump"); rec.Code != http.StatusOK {
		t.Errorf("HEAD of a real object = %d, want 200", rec.Code)
	}
	if rec := objectRequest(t, s, http.MethodGet, bucket, "postgres/db-2026-09-20.dump"); rec.Code != http.StatusOK {
		t.Errorf("GET of a real object = %d, want 200", rec.Code)
	}
}
