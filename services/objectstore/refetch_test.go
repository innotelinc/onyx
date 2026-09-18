package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// dyingFetchCloud is a transport whose download dies part way through: it
// writes the bytes it managed to get and then reports the failure, which is
// exactly what an interrupted `rclone copyto` leaves on disk. `dead` toggles it
// so one test can watch a failed refetch and then the retry that fixes it.
type dyingFetchCloud struct {
	*fakeCloud
	dead    bool
	partial []byte
	err     error
}

func (d *dyingFetchCloud) Fetch(ctx context.Context, target, localPath string) error {
	if !d.dead {
		return d.fakeCloud.Fetch(ctx, target, localPath)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(localPath, d.partial, 0o640); err != nil {
		return err
	}
	return d.err
}

// evict puts one object in the cloud and releases its local copy, returning the
// bucket's directory.
func evict(t *testing.T, s *server, cloud *fakeCloud, bucket, key, body string) string {
	t.Helper()
	mustPut(t, s, bucket, key, body)
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.objects, bucket, key), old, old); err != nil {
		t.Fatal(err)
	}
	resp, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: bucket, Evict: true})
	if err != nil {
		t.Fatalf("SyncBucket: %v", err)
	}
	if resp.GetEvicted() == 0 {
		t.Fatalf("nothing was evicted: %+v", resp)
	}
	if _, err := os.Stat(filepath.Join(s.objects, bucket, key)); !os.IsNotExist(err) {
		t.Fatalf("the local copy survived the eviction: %v", err)
	}
	if _, ok := cloud.files["b2-archive:"+cloudObjectPrefix+"/"+bucket+"/"+key]; !ok {
		t.Fatal("the evicted object is not in the cloud, so nothing can be refetched")
	}
	return filepath.Join(s.objects, bucket, key)
}

// A refetch that fails must leave the object evicted, not corrupted.
//
// The cache is read before the cloud on every GET: a partial file written to
// the object's path would be served as the object from then on, with no error
// and no further attempt at the cloud. This is the one way an eviction can lose
// data that the cloud still holds.
func TestRefetchFailureLeavesNoPartialObjectInTheCache(t *testing.T) {
	cloud := &dyingFetchCloud{
		fakeCloud: newFakeCloud(),
		dead:      true,
		partial:   []byte("the whole"), // half of the body below
		err:       errors.New("connection reset by peer"),
	}
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{
		Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 1,
	})
	path := evict(t, s, cloud.fakeCloud, "tiered", "cold.txt", "the whole object")

	_, err := s.GetObject(context.Background(), &onyxv1.GetObjectRequest{Bucket: "tiered", Key: "cold.txt"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v, want NotFound when the cloud cannot be reached", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		// The killer case: the truncated bytes are now the object.
		if statErr == nil {
			body, _ := os.ReadFile(path)
			t.Fatalf("a failed refetch left %q at the object's path; the next read would serve it", body)
		}
		t.Fatalf("unexpected cache state after a failed refetch: %v", statErr)
	}
	// The temp download goes in the store's hidden area, so bucket listings
	// never see it — but it must not be left behind either.
	tmpDir := filepath.Join(s.userMetaDir("tiered"), "tmp")
	if entries, readErr := os.ReadDir(tmpDir); readErr == nil && len(entries) != 0 {
		t.Errorf("a failed refetch left %d temp file(s) behind: %v", len(entries), entries)
	}

	// The object is still there to be fetched: a failed refetch is a retryable
	// state, not a lost object.
	cloud.dead = false
	obj, err := s.GetObject(context.Background(), &onyxv1.GetObjectRequest{Bucket: "tiered", Key: "cold.txt"})
	if err != nil {
		t.Fatalf("GetObject after the transport recovered: %v", err)
	}
	if string(obj.GetData()) != "the whole object" {
		t.Errorf("data = %q, want the full body", obj.GetData())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the refetched object was not cached: %v", err)
	}
}

// Evicting the local copy does not delete the object — it lives in the cloud
// and the next read brings it back — so the client metadata stored alongside it
// has to come back too. A client that PUT a document with its hash in
// `x-amz-meta-*` checks that hash on the way out; an eviction that dropped the
// sidecar would answer the refetched object's HEAD with no metadata at all.
func TestEvictionKeepsUserMetadataForARefetchedObject(t *testing.T) {
	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{
		Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 1,
	})
	if err := s.saveUserMeta("tiered", "cold.txt", map[string]string{"sha256": "abc123"}); err != nil {
		t.Fatalf("saveUserMeta: %v", err)
	}
	evict(t, s, cloud, "tiered", "cold.txt", "body")

	if got := s.loadUserMeta("tiered", "cold.txt"); got["sha256"] != "abc123" {
		t.Fatalf("metadata after eviction = %v, want sha256=abc123", got)
	}
	if _, err := s.GetObject(context.Background(), &onyxv1.GetObjectRequest{Bucket: "tiered", Key: "cold.txt"}); err != nil {
		t.Fatalf("GetObject after eviction: %v", err)
	}
	if got := s.loadUserMeta("tiered", "cold.txt"); got["sha256"] != "abc123" {
		t.Errorf("metadata after the refetch = %v, want sha256=abc123", got)
	}
}

// The console, the CLI and most S3 clients probe with HEAD before they download.
// HEAD on an evicted object therefore has to refetch it: answering 404 while GET
// succeeds tells every one of them the object is gone.
func TestS3HeadOnAnEvictedObjectRefetchesIt(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY", "")
	t.Setenv("S3_SECRET_KEY", "")

	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{
		Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 1,
	})
	path := evict(t, s, cloud, "tiered", "cold.txt", "cold-body")
	handler := newS3Handler(s, true)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "http://storage/tiered/cold.txt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD on an evicted object: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	sum := md5.Sum([]byte("cold-body"))
	if want := `"` + hex.EncodeToString(sum[:]) + `"`; rec.Header().Get("ETag") != want {
		t.Errorf("ETag = %s, want %s", rec.Header().Get("ETag"), want)
	}
	if got := rec.Header().Get("Content-Length"); got != "9" {
		t.Errorf("Content-Length = %s, want 9", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("HEAD refetched but did not cache the object: %v", err)
	}

	// An evicted object the cloud no longer holds is a clean 404 (a client that
	// knows the difference between "gone" and "the store broke" can act on it).
	gone := evict(t, s, cloud, "tiered", "gone.txt", "vanished")
	delete(cloud.files, "b2-archive:"+cloudObjectPrefix+"/tiered/gone.txt")
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("the second object was not evicted: %v", err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "http://storage/tiered/gone.txt", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("HEAD on an object the cloud does not hold: status = %d, want 404", rec.Code)
	}
}
