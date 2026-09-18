package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// fakeCloud is an in-memory cloud target: enough to check the tiering policy
// without rclone or a network. It records what was asked so the tests can assert
// the order and shape of the calls, and lets each operation fail on demand.
type fakeCloud struct {
	files    map[string][]byte
	remotes  []string
	known    bool
	uploads  []string
	purged   []string
	removed  []string
	fetches  []string
	copyErr  error
	upErr    error
	verErr   error
	fetchErr error
	purgeErr error
	sizeErr  error
}

// newFakeCloud stands in for a configured remote catalog: `b2-archive` exists,
// so a bucket can target it the way a real deployment would.
func newFakeCloud() *fakeCloud {
	return &fakeCloud{files: map[string][]byte{}, known: true, remotes: []string{"b2-archive"}}
}

func (f *fakeCloud) Upload(_ context.Context, localPath, target string) error {
	if f.upErr != nil {
		return f.upErr
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	f.files[target] = data
	f.uploads = append(f.uploads, target)
	return nil
}

func (f *fakeCloud) Copy(_ context.Context, localPath, target string) (string, error) {
	if f.copyErr != nil {
		return "", f.copyErr
	}
	var keys []string
	err := filepath.WalkDir(localPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == metaDirName {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(localPath, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f.files[target+"/"+filepath.ToSlash(rel)] = data
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(keys)
	return fmt.Sprintf("transferred %d file(s)", len(keys)), nil
}

func (f *fakeCloud) Verify(_ context.Context, localPath, target string) error {
	return f.verErr
}

func (f *fakeCloud) Fetch(_ context.Context, target, localPath string) error {
	if f.fetchErr != nil {
		return f.fetchErr
	}
	data, ok := f.files[target]
	if !ok {
		return errors.New("object not found in the cloud target")
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
		return err
	}
	f.fetches = append(f.fetches, target)
	return os.WriteFile(localPath, data, 0o640)
}

func (f *fakeCloud) Remove(_ context.Context, target string) error {
	f.removed = append(f.removed, target)
	delete(f.files, target)
	return nil
}

func (f *fakeCloud) Purge(_ context.Context, target string) error {
	if f.purgeErr != nil {
		return f.purgeErr
	}
	f.purged = append(f.purged, target)
	for key := range f.files {
		if strings.HasPrefix(key, target) {
			delete(f.files, key)
		}
	}
	return nil
}

func (f *fakeCloud) Size(_ context.Context, target string) (int64, int64, error) {
	if f.sizeErr != nil {
		return 0, 0, f.sizeErr
	}
	var count, bytes int64
	for key, data := range f.files {
		if strings.HasPrefix(key, target) {
			count++
			bytes += int64(len(data))
		}
	}
	return count, bytes, nil
}

func (f *fakeCloud) Remotes() ([]string, bool) { return f.remotes, f.known }

func tieredServer(t *testing.T, cloud cloudTransport) (*server, string) {
	t.Helper()
	stateDir := t.TempDir()
	s, err := newServer(stateDir, cloud)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return s, stateDir
}

func mustCreateBucket(t *testing.T, s *server, req *onyxv1.CreateBucketRequest) *onyxv1.Bucket {
	t.Helper()
	b, err := s.CreateBucket(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBucket(%s): %v", req.GetName(), err)
	}
	return b
}

func mustPut(t *testing.T, s *server, bucket, key, body string) {
	t.Helper()
	if _, err := s.PutObject(context.Background(), &onyxv1.PutObjectRequest{
		Bucket: bucket, Key: key, Data: []byte(body),
	}); err != nil {
		t.Fatalf("PutObject(%s/%s): %v", bucket, key, err)
	}
}

// The `.meta` tree holds user metadata *about* objects. It must never appear in
// a bucket listing: a client would see keys it never wrote, and anything that
// trusted the listing (a backup, an inventory, an age-out sweep) would act on
// them.
func TestWalkObjectKeysSkipsMetadataSidecars(t *testing.T) {
	objects := t.TempDir()
	writeObject(t, objects, "docs", "a.pdf", "a")
	writeObject(t, objects, "docs", "nested/b.pdf", "b")
	if err := os.MkdirAll(filepath.Join(objects, "docs", metaDirName, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objects, "docs", metaDirName, "a.pdf.json"), []byte(`{"hash":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objects, "docs", metaDirName, "nested", "b.pdf.json"), []byte(`{"hash":"y"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := listObjects(t, objects, "docs", "")
	if code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", code, body)
	}
	if strings.Contains(body, metaDirName) {
		t.Errorf("the metadata sidecar leaked into the listing\n%s", body)
	}
	if !strings.Contains(body, "<KeyCount>2</KeyCount>") {
		t.Errorf("KeyCount should count only real objects\n%s", body)
	}
}

func TestCreateBucketValidatesCloudTarget(t *testing.T) {
	cloud := newFakeCloud()
	cloud.remotes = []string{"b2-archive"}
	s, _ := tieredServer(t, cloud)
	ctx := context.Background()

	cases := []struct {
		name string
		req  *onyxv1.CreateBucketRequest
	}{
		{"local needs no target", &onyxv1.CreateBucketRequest{Name: "local-only"}},
		{"cloud without a target", &onyxv1.CreateBucketRequest{Name: "no-target", Tier: onyxv1.BucketTier_CLOUD}},
		{"target that is a flag", &onyxv1.CreateBucketRequest{Name: "flaggy", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "--evil"}},
		{"target with newline", &onyxv1.CreateBucketRequest{Name: "newline", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "b2-archive:\nx"}},
		{"two colons", &onyxv1.CreateBucketRequest{Name: "colons", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "a:b:c"}},
		// A bare name is ambiguous, and reading it as a relative directory is
		// the dangerous way to be wrong.
		{"bare name with no colon", &onyxv1.CreateBucketRequest{Name: "bare", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "b2-archive"}},
		{"unknown remote", &onyxv1.CreateBucketRequest{Name: "typo", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "b2-archiv:"}},
		{"empty remote name", &onyxv1.CreateBucketRequest{Name: "empty", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: ":path"}},
		{"cloud cannot evict", &onyxv1.CreateBucketRequest{Name: "evicting-cloud", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "b2-archive:", EvictAfterDays: 7}},
	}
	for _, c := range cases {
		if c.name == "local needs no target" {
			if _, err := s.CreateBucket(ctx, c.req); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if _, err := s.CreateBucket(ctx, c.req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", c.name, err)
		}
	}

	// A local directory target is legitimate (how tiering is tested on the pool).
	if _, err := s.CreateBucket(ctx, &onyxv1.CreateBucketRequest{
		Name: "local-target", Tier: onyxv1.BucketTier_TIERED, CloudTarget: t.TempDir(),
	}); err != nil {
		t.Errorf("a plain directory target should be accepted: %v", err)
	}
}

// Without a transport, a cloud bucket must be refused rather than quietly
// keeping the data local: the operator asked for the cloud.
func TestCloudTiersRefusedWithoutATransport(t *testing.T) {
	s, _ := tieredServer(t, nil)
	_, err := s.CreateBucket(context.Background(), &onyxv1.CreateBucketRequest{
		Name: "cloudbucket", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "b2-archive:",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

// CLOUD is cloud-primary: a write that the cloud did not accept is not a
// success, and it leaves nothing behind locally.
func TestCloudPutFailsWhenTheCloudRejects(t *testing.T) {
	cloud := newFakeCloud()
	cloud.upErr = errors.New("403 Forbidden")
	s, stateDir := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "cloudbucket", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "b2-archive:"})

	_, err := s.PutObject(context.Background(), &onyxv1.PutObjectRequest{Bucket: "cloudbucket", Key: "a.txt", Data: []byte("hello")})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(stateDir, "objects", "cloudbucket"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("a rejected cloud write left %d local entries", len(entries))
	}
}

func TestCloudPutUploadsAndDoesNotKeepALocalCopy(t *testing.T) {
	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "cloudbucket", Tier: onyxv1.BucketTier_CLOUD, CloudTarget: "b2-archive:"})
	mustPut(t, s, "cloudbucket", "docs/a.txt", "hello")

	want := "b2-archive:" + cloudObjectPrefix + "/cloudbucket/docs/a.txt"
	if len(cloud.uploads) != 1 || cloud.uploads[0] != want {
		t.Fatalf("uploads = %v, want [%s]", cloud.uploads, want)
	}
	if _, err := os.Stat(filepath.Join(s.objects, "cloudbucket", "docs", "a.txt")); !os.IsNotExist(err) {
		t.Error("a CLOUD bucket kept a local copy of a fresh write")
	}
}

// A TIERED bucket acknowledges a write from the hot copy: the point of a hot
// tier is that a slow cloud is not on the write path.
func TestTieredPutIsLocalOnly(t *testing.T) {
	cloud := newFakeCloud()
	cloud.upErr = errors.New("network unreachable")
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 30})
	mustPut(t, s, "tiered", "docs/a.txt", "hello")

	if len(cloud.uploads) != 0 {
		t.Errorf("a TIERED put should not touch the cloud synchronously: %v", cloud.uploads)
	}
	if _, err := os.Stat(filepath.Join(s.objects, "tiered", "docs", "a.txt")); err != nil {
		t.Errorf("the hot copy is missing: %v", err)
	}
}

func TestSyncUploadsAndRecordsState(t *testing.T) {
	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 30})
	mustPut(t, s, "tiered", "docs/a.txt", "a")
	mustPut(t, s, "tiered", "docs/b.txt", "bb")

	resp, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: "tiered"})
	if err != nil {
		t.Fatalf("SyncBucket: %v", err)
	}
	if resp.GetUploaded() != 2 {
		t.Errorf("uploaded = %d, want 2", resp.GetUploaded())
	}
	for _, key := range []string{"docs/a.txt", "docs/b.txt"} {
		want := "b2-archive:" + cloudObjectPrefix + "/tiered/" + key
		if _, ok := cloud.files[want]; !ok {
			t.Errorf("cloud is missing %s", want)
		}
	}
	if resp.GetBucket().GetCloudObjects() != 2 || resp.GetBucket().GetLastSyncAt() == "" {
		t.Errorf("bucket state not recorded: %+v", resp.GetBucket())
	}
	if resp.GetEvicted() != 0 {
		t.Errorf("evicted = %d, want 0 when eviction was not requested", resp.GetEvicted())
	}
	// Nothing was evicted, so the hot copies are untouched.
	if _, err := os.Stat(filepath.Join(s.objects, "tiered", "docs", "a.txt")); err != nil {
		t.Errorf("hot copy disappeared without eviction: %v", err)
	}
}

// Eviction is the one destructive step in a sync, so it happens only when the
// whole bucket verified — never on a partial or failed check.
func TestSyncRefusesToEvictUnverifiedCopies(t *testing.T) {
	cloud := newFakeCloud()
	cloud.verErr = errors.New("2 files differ")
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 1})
	mustPut(t, s, "tiered", "docs/old.txt", "old")
	// Age the object past the eviction window.
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.objects, "tiered", "docs", "old.txt"), old, old); err != nil {
		t.Fatal(err)
	}

	resp, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: "tiered", Evict: true})
	if err != nil {
		t.Fatalf("SyncBucket: %v", err)
	}
	if resp.GetEvicted() != 0 {
		t.Errorf("evicted = %d, want 0 when verification failed", resp.GetEvicted())
	}
	if len(resp.GetWarnings()) == 0 {
		t.Error("a failed verification must be reported as a warning")
	}
	if _, err := os.Stat(filepath.Join(s.objects, "tiered", "docs", "old.txt")); err != nil {
		t.Errorf("an unverified copy was evicted: %v", err)
	}
}

func TestSyncEvictsVerifiedCopiesPastTheAge(t *testing.T) {
	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 1})
	mustPut(t, s, "tiered", "cold.txt", "cold")
	mustPut(t, s, "tiered", "hot.txt", "hot")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.objects, "tiered", "cold.txt"), old, old); err != nil {
		t.Fatal(err)
	}

	resp, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: "tiered", Evict: true})
	if err != nil {
		t.Fatalf("SyncBucket: %v", err)
	}
	if resp.GetEvicted() != 1 {
		t.Fatalf("evicted = %d, want 1 (the aged object only)", resp.GetEvicted())
	}
	if _, err := os.Stat(filepath.Join(s.objects, "tiered", "cold.txt")); !os.IsNotExist(err) {
		t.Error("the aged copy was not evicted")
	}
	if _, err := os.Stat(filepath.Join(s.objects, "tiered", "hot.txt")); err != nil {
		t.Errorf("a recent copy was evicted: %v", err)
	}
	if _, ok := cloud.files["b2-archive:"+cloudObjectPrefix+"/tiered/cold.txt"]; !ok {
		t.Error("the evicted object is not in the cloud")
	}
}

// Eviction must not lose data for a reader: an evicted object is fetched back
// from the cloud on demand.
func TestGetRefetchesAnEvictedObject(t *testing.T) {
	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 1})
	mustPut(t, s, "tiered", "cold.txt", "cold-body")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.objects, "tiered", "cold.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: "tiered", Evict: true}); err != nil {
		t.Fatalf("SyncBucket: %v", err)
	}

	obj, err := s.GetObject(context.Background(), &onyxv1.GetObjectRequest{Bucket: "tiered", Key: "cold.txt"})
	if err != nil {
		t.Fatalf("GetObject after eviction: %v", err)
	}
	if string(obj.GetData()) != "cold-body" {
		t.Errorf("data = %q, want the evicted body", obj.GetData())
	}
	if len(cloud.fetches) != 1 {
		t.Errorf("fetches = %v, want one refetch", cloud.fetches)
	}

	// A LOCAL bucket has nowhere to refetch from, so a missing object stays a
	// clean NotFound rather than a cloud error.
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "plain"})
	if _, err := s.GetObject(context.Background(), &onyxv1.GetObjectRequest{Bucket: "plain", Key: "absent.txt"}); status.Code(err) != codes.NotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestDeleteRemovesTheCloudCopy(t *testing.T) {
	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 30})
	mustPut(t, s, "tiered", "docs/a.txt", "a")
	if _, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: "tiered"}); err != nil {
		t.Fatalf("SyncBucket: %v", err)
	}

	if _, err := s.DeleteObject(context.Background(), &onyxv1.DeleteObjectRequest{Bucket: "tiered", Key: "docs/a.txt"}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, ok := cloud.files["b2-archive:"+cloudObjectPrefix+"/tiered/docs/a.txt"]; ok {
		t.Error("the cloud copy survived a delete")
	}
	if _, err := os.Stat(filepath.Join(s.objects, "tiered", "docs", "a.txt")); !os.IsNotExist(err) {
		t.Error("the local copy survived a delete")
	}
}

// Deleting a cloud bucket has to release the cloud side too, or the objects
// outlive the bucket that names them with nothing left to reach them.
func TestDeleteBucketPurgesTheCloudTarget(t *testing.T) {
	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 30})
	mustPut(t, s, "tiered", "docs/a.txt", "a")
	if _, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: "tiered"}); err != nil {
		t.Fatalf("SyncBucket: %v", err)
	}

	if _, err := s.DeleteBucket(context.Background(), &onyxv1.DeleteBucketRequest{Name: "tiered", Force: true}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	want := "b2-archive:" + cloudObjectPrefix + "/tiered"
	if len(cloud.purged) != 1 || cloud.purged[0] != want {
		t.Fatalf("purged = %v, want [%s]", cloud.purged, want)
	}
	if _, ok := cloud.files[want+"/docs/a.txt"]; ok {
		t.Error("cloud objects survived the bucket delete")
	}
}

// A cloud purge that fails must leave the bucket in place: the objects are still
// reachable, so reporting success would strand them.
func TestDeleteBucketKeepsTheBucketWhenTheCloudPurgeFails(t *testing.T) {
	cloud := newFakeCloud()
	cloud.purgeErr = errors.New("connection reset")
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 30})
	mustPut(t, s, "tiered", "docs/a.txt", "a")

	if _, err := s.DeleteBucket(context.Background(), &onyxv1.DeleteBucketRequest{Name: "tiered", Force: true}); err == nil {
		t.Fatal("expected the failed cloud purge to surface")
	}
	if _, err := os.Stat(filepath.Join(s.objects, "tiered", "docs", "a.txt")); err != nil {
		t.Errorf("the local objects were deleted despite a failed cloud purge: %v", err)
	}
}

func TestSyncRejectsLocalBucketsAndReportsCloudFailures(t *testing.T) {
	cloud := newFakeCloud()
	cloud.copyErr = errors.New("no such remote")
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "plain"})
	if _, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: "plain"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("sync of a LOCAL bucket: %v, want FailedPrecondition", err)
	}

	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "tiered", Tier: onyxv1.BucketTier_TIERED, CloudTarget: "b2-archive:", EvictAfterDays: 30})
	if _, err := s.SyncBucket(context.Background(), &onyxv1.SyncBucketRequest{Name: "tiered"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("sync with a broken transport: %v, want Unavailable", err)
	}
	// The failure is recorded so the bucket's own state tells the operator, not
	// only whoever ran the failed request.
	list, err := s.ListBuckets(context.Background(), &onyxv1.ListBucketsRequest{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	for _, b := range list.GetBuckets() {
		if b.GetName() == "tiered" && b.GetSyncError() == "" {
			t.Error("the sync failure was not recorded on the bucket")
		}
	}
}

// The bucket's local count is refreshed on every listing, because it is a cheap
// directory walk; the cloud count is a recorded observation, because counting it
// means a network round trip.
func TestListBucketsCountsLocalObjects(t *testing.T) {
	cloud := newFakeCloud()
	s, _ := tieredServer(t, cloud)
	mustCreateBucket(t, s, &onyxv1.CreateBucketRequest{Name: "plain"})
	mustPut(t, s, "plain", "a.txt", "a")
	mustPut(t, s, "plain", "nested/b.txt", "b")

	list, err := s.ListBuckets(context.Background(), &onyxv1.ListBucketsRequest{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(list.GetBuckets()) != 1 || list.GetBuckets()[0].GetLocalObjects() != 2 {
		t.Fatalf("buckets = %+v, want a local count of 2", list.GetBuckets())
	}
}
