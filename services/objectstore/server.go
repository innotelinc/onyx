package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and ObjectStore (proto/onyx/v1/objectstore.proto).
// Bucket metadata persists as <state-dir>/buckets.json; objects live under
// <state-dir>/objects/<bucket>/<key>.
//
// Hybrid cloud (docs/design/11 §6.6): a bucket is LOCAL, CLOUD or TIERED.
//   - LOCAL   — every byte stays on the pool.
//   - CLOUD   — the external target is primary: a write is only acknowledged
//     once the cloud holds it, and a read of an object that is not cached
//     locally is served from the cloud.
//   - TIERED  — local is the hot tier and the cloud the cold tier: writes land
//     locally for speed, SyncBucket mirrors them out, and a verified cloud copy
//     lets the local copy be evicted once the bucket's evict_after_days has
//     passed. A read of an evicted object transparently refetches it.
//
// A local copy is a cache in both cloud tiers, never the only copy of anything
// the caller was told was written: nothing is evicted before `rclone check`
// confirms the cloud copy, and a failed cloud write fails the request.
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedObjectStoreServer

	mu       sync.Mutex
	stateDir string
	objects  string
	buckets  map[string]*onyxv1.Bucket
	// cloud is nil when no remote catalog is configured: cloud tiers then
	// refuse work with a clear message instead of silently keeping data local.
	cloud cloudTransport
	// now is the clock eviction age is measured against (test seam).
	now func() time.Time
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.ObjectStoreServer = (*server)(nil)

func newServer(stateDir string, cloud cloudTransport) (*server, error) {
	s := &server{
		stateDir: stateDir,
		objects:  filepath.Join(stateDir, "objects"),
		buckets:  map[string]*onyxv1.Bucket{},
		cloud:    cloud,
		now:      time.Now,
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "buckets.json"))
	if err == nil {
		if err := json.Unmarshal(raw, &s.buckets); err != nil {
			return nil, fmt.Errorf("parse buckets.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *server) persistLocked() error {
	raw, err := json.MarshalIndent(s.buckets, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.stateDir, "buckets.json"), raw, 0o640)
}

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

func (s *server) ListBuckets(_ context.Context, _ *onyxv1.ListBucketsRequest) (*onyxv1.ListBucketsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*onyxv1.Bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		// The local count is a directory walk — cheap and always current. The
		// cloud count is deliberately *not* refreshed here: counting the remote
		// means a network round trip, and this call backs both the dashboard
		// and every S3 client's ListBuckets. SyncBucket records it instead.
		if keys, err := walkObjectKeys(filepath.Join(s.objects, b.Name)); err == nil {
			b.LocalObjects = int64(len(keys))
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &onyxv1.ListBucketsResponse{Buckets: out}, nil
}

func (s *server) CreateBucket(_ context.Context, req *onyxv1.CreateBucketRequest) (*onyxv1.Bucket, error) {
	if !validBucketName(req.GetName()) {
		return nil, status.Error(codes.InvalidArgument, "invalid bucket name (3-63 chars, lowercase, dots/dashes)")
	}
	tier := req.GetTier()
	if tier == onyxv1.BucketTier_BUCKET_TIER_UNSPECIFIED {
		tier = onyxv1.BucketTier_LOCAL
	}
	if tier != onyxv1.BucketTier_LOCAL {
		if err := validateCloudTarget(req.GetCloudTarget()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if s.cloud == nil {
			return nil, status.Error(codes.FailedPrecondition, "no cloud transport is configured: set --rclone-bin and mount the remote catalog, then retry")
		}
		if remote := cloudRemoteName(req.GetCloudTarget()); remote != "" && !s.remoteExists(remote) {
			return nil, status.Errorf(codes.InvalidArgument, "remote %q is not configured in the rclone catalog", remote)
		}
	}
	if req.GetEvictAfterDays() < 0 {
		return nil, status.Error(codes.InvalidArgument, "evict_after_days must not be negative")
	}
	if tier == onyxv1.BucketTier_CLOUD && req.GetEvictAfterDays() > 0 {
		return nil, status.Error(codes.InvalidArgument, "CLOUD buckets hold no tier to evict from; evict_after_days applies to TIERED buckets")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[req.GetName()]; ok {
		return nil, status.Error(codes.AlreadyExists, "bucket exists")
	}
	b := &onyxv1.Bucket{
		Name:        req.GetName(),
		Tier:        tier,
		CloudTarget: req.GetCloudTarget(),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if tier == onyxv1.BucketTier_TIERED {
		b.EvictAfterDays = req.GetEvictAfterDays()
	}
	if err := os.MkdirAll(filepath.Join(s.objects, b.Name), 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.buckets[b.Name] = b
	if err := s.persistLocked(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return b, nil
}

func (s *server) DeleteBucket(ctx context.Context, req *onyxv1.DeleteBucketRequest) (*onyxv1.DeleteBucketResponse, error) {
	s.mu.Lock()
	b, ok := s.buckets[req.GetName()]
	if !ok {
		s.mu.Unlock()
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	dir := filepath.Join(s.objects, b.Name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		s.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	if len(entries) > 0 && !req.GetForce() {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "bucket is not empty (use force)")
	}
	cloud := s.cloud
	target := cloudTargetFor(b.CloudTarget, b.Name)
	tiered := b.Tier != onyxv1.BucketTier_LOCAL
	delete(s.buckets, b.Name)
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	s.mu.Unlock()

	// The cloud side goes first: once the local directory is gone, a failed
	// cloud purge would leave objects nobody can reach or name.
	if tiered && cloud != nil && b.CloudTarget != "" {
		if err := cloud.Purge(ctx, target); err != nil {
			return nil, status.Errorf(codes.Internal, "purge cloud target %s: %v", target, err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &onyxv1.DeleteBucketResponse{Deleted: true}, nil
}

func (s *server) PutObject(ctx context.Context, req *onyxv1.PutObjectRequest) (*onyxv1.ObjectMeta, error) {
	if req.GetBucket() == "" || req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket and key are required")
	}
	s.mu.Lock()
	b, ok := s.buckets[req.GetBucket()]
	if !ok {
		s.mu.Unlock()
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	path, err := s.objectPathLocked(req.GetBucket(), req.GetKey())
	cloud := s.cloud
	tier, target := b.Tier, cloudTargetFor(b.CloudTarget, b.Name)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.WriteFile(path, req.GetData(), 0o640); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	meta := &onyxv1.ObjectMeta{
		Bucket:       req.GetBucket(),
		Key:          req.GetKey(),
		SizeBytes:    int64(len(req.GetData())),
		Etag:         objectETag(req.GetData()),
		ContentType:  req.GetContentType(),
		LastModified: time.Now().UTC().Format(time.RFC3339),
	}

	switch {
	case tier == onyxv1.BucketTier_CLOUD:
		// The cloud is primary here, so a write is not acknowledged until the
		// cloud holds it. A local-only success would be a lie the next reader
		// would discover.
		if cloud == nil {
			_ = os.Remove(path)
			return nil, status.Error(codes.FailedPrecondition, "bucket tier is CLOUD but no cloud transport is configured")
		}
		if err := cloudUpload(ctx, cloud, path, target, req.GetKey()); err != nil {
			_ = os.Remove(path)
			return nil, status.Errorf(codes.Unavailable, "cloud write failed: %v", err)
		}
		_ = os.Remove(path) // CLOUD keeps no local copy beyond what the write needed
	case tier == onyxv1.BucketTier_TIERED:
		// The hot copy is the acknowledgement; SyncBucket mirrors it out. A
		// local success that never reaches the cloud is still a success for the
		// writer, which is the whole point of a hot tier.
		if cloud == nil {
			return nil, status.Error(codes.FailedPrecondition, "bucket tier is TIERED but no cloud transport is configured")
		}
	}
	return meta, nil
}

func (s *server) GetObject(ctx context.Context, req *onyxv1.GetObjectRequest) (*onyxv1.GetObjectResponse, error) {
	s.mu.Lock()
	path, err := s.objectPathLocked(req.GetBucket(), req.GetKey())
	b := s.buckets[req.GetBucket()]
	cloud := s.cloud
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// The bucket's own directory is not the only path that can land here: a key
	// that names a prefix does too, and reading it would fail with EISDIR and
	// answer 500 for a key that does not exist. See errNotAnObject.
	if _, statErr := statObjectFile(path); errors.Is(statErr, errNotAnObject) {
		return nil, status.Error(codes.NotFound, "object not found")
	}
	data, readErr := os.ReadFile(path)
	if readErr == nil {
		return &onyxv1.GetObjectResponse{
			Meta: &onyxv1.ObjectMeta{Bucket: req.GetBucket(), Key: req.GetKey(), SizeBytes: int64(len(data))},
			Data: data,
		}, nil
	}
	if !os.IsNotExist(readErr) {
		return nil, status.Error(codes.Internal, readErr.Error())
	}
	// Not cached locally. For a cloud tiered bucket the cloud copy is the
	// object — that is exactly the state the local cache is allowed to be in
	// after an eviction, so a read refetches instead of reporting NotFound.
	if b == nil || b.Tier == onyxv1.BucketTier_LOCAL || cloud == nil {
		return nil, status.Error(codes.NotFound, "object not found")
	}
	target := cloudTargetFor(b.CloudTarget, b.Name) + "/" + cloudPath(req.GetKey())
	if err := s.refetchObject(ctx, cloud, req.GetBucket(), req.GetKey(), target, path); err != nil {
		return nil, status.Errorf(codes.NotFound, "object not found locally and not retrievable from the cloud target: %v", err)
	}
	data, readErr = os.ReadFile(path)
	if readErr != nil {
		return nil, status.Error(codes.Internal, readErr.Error())
	}
	return &onyxv1.GetObjectResponse{
		Meta: &onyxv1.ObjectMeta{Bucket: req.GetBucket(), Key: req.GetKey(), SizeBytes: int64(len(data))},
		Data: data,
	}, nil
}

// refetchObject downloads an evicted object back into the cache so that the
// cache only ever holds whole objects.
//
// The transfer has to go through a temporary path: `rclone copyto` (and every
// transport shaped like it) writes the destination as the bytes arrive, so a
// download that dies halfway — a reset connection, the transport's own timeout,
// a cancelled request — leaves a truncated file *at the object's path*. From
// then on every read finds a local file, serves it as the object, and never
// asks the cloud again: the cache would silently hold a corrupt copy of data
// that is intact in the cloud. Downloading into the store's hidden `.meta` area
// (which bucket listings never walk, so a temp can never be mistaken for an
// object) and renaming into place on success makes the object appear only once
// it is complete — a failed refetch leaves the object evicted and retryable.
func (s *server) refetchObject(ctx context.Context, cloud cloudTransport, bucket, key, target, path string) error {
	tmpRoot := filepath.Join(s.userMetaDir(bucket), "tmp")
	if err := os.MkdirAll(tmpRoot, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpRoot, "refetch-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if closeErr := tmp.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := cloud.Fetch(ctx, target, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// userMetaDir is where per-object user metadata lives: a hidden directory inside
// the bucket, mirroring the key path. It is deliberately a directory rather than
// a `<key>.meta.json` sibling because bucket listing skips it, so the sidecar
// never shows up as an object.
func (s *server) userMetaDir(bucket string) string {
	return filepath.Join(s.objects, bucket, ".meta")
}

func (s *server) userMetaPath(bucket, key string) string {
	return filepath.Join(s.userMetaDir(bucket), filepath.Clean(key)+".json")
}

// saveUserMeta records the `x-amz-meta-*` headers a client sent with an object.
// S3 keeps these alongside the object and returns them on HEAD/GET; storing them
// is what lets a client check a downloaded document against the hash it uploaded
// without a second lookup.
func (s *server) saveUserMeta(bucket, key string, meta map[string]string) error {
	if len(meta) == 0 {
		return nil
	}
	path := s.userMetaPath(bucket, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o640)
}

// loadUserMeta returns the stored metadata, or nil when there is none. A missing
// or unreadable sidecar is not an error: the object is still the object.
func (s *server) loadUserMeta(bucket, key string) map[string]string {
	raw, err := os.ReadFile(s.userMetaPath(bucket, key))
	if err != nil {
		return nil
	}
	meta := map[string]string{}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil
	}
	return meta
}

func (s *server) DeleteObject(ctx context.Context, req *onyxv1.DeleteObjectRequest) (*onyxv1.DeleteObjectResponse, error) {
	s.mu.Lock()
	path, err := s.objectPathLocked(req.GetBucket(), req.GetKey())
	b := s.buckets[req.GetBucket()]
	cloud := s.cloud
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// The cloud copy goes first for a cloud tier: if it fails, the object is
	// still fully present and the caller can retry, instead of the local copy
	// being gone from a bucket whose listing is built from the cloud.
	if b != nil && b.Tier != onyxv1.BucketTier_LOCAL && cloud != nil {
		target := cloudTargetFor(b.CloudTarget, b.Name) + "/" + cloudPath(req.GetKey())
		if err := cloud.Remove(ctx, target); err != nil {
			// A cloud object that was never uploaded is not a failure.
			if !strings.Contains(err.Error(), "object not found") && !strings.Contains(err.Error(), "not found") {
				return nil, status.Errorf(codes.Unavailable, "cloud delete failed: %v", err)
			}
		}
	}
	removeErr := os.Remove(path)
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return nil, status.Error(codes.Internal, removeErr.Error())
	}
	// The sidecar goes with the object, or a later PUT under the same key would
	// inherit the previous object's metadata.
	_ = os.Remove(s.userMetaPath(req.GetBucket(), req.GetKey()))
	return &onyxv1.DeleteObjectResponse{Deleted: removeErr == nil}, nil
}

// SyncBucket mirrors a cloud-tiered bucket into its cloud target and optionally
// releases local copies that the cloud has been verified to hold.
func (s *server) SyncBucket(ctx context.Context, req *onyxv1.SyncBucketRequest) (*onyxv1.SyncBucketResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket name is required")
	}
	s.mu.Lock()
	b, ok := s.buckets[req.GetName()]
	cloud := s.cloud
	s.mu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	if b.Tier == onyxv1.BucketTier_LOCAL {
		return nil, status.Errorf(codes.FailedPrecondition, "bucket %s is LOCAL: it has no cloud target to sync with", b.Name)
	}
	if cloud == nil {
		return nil, status.Error(codes.FailedPrecondition, "no cloud transport is configured")
	}
	dir := filepath.Join(s.objects, b.Name)
	target := cloudTargetFor(b.CloudTarget, b.Name)

	resp := &onyxv1.SyncBucketResponse{Warnings: []string{}}
	// The count on the cloud side before the copy is what makes "uploaded" mean
	// something: the difference between the two readings is the number of
	// objects this sync actually added. The cloud count is not tracked locally,
	// so it has to be read — and a sync is a deliberate action, so the two
	// round trips are affordable.
	beforeCloud, _, beforeErr := cloud.Size(ctx, target)

	detail, err := cloud.Copy(ctx, dir, target)
	resp.Detail = detail
	if err != nil {
		s.recordSyncFailure(b.Name, err)
		return nil, status.Errorf(codes.Unavailable, "cloud sync failed: %v", err)
	}

	// Verification is what makes eviction safe, so it runs before any local
	// copy is released — and its failure cancels eviction entirely rather than
	// touching files the cloud may not hold.
	verified := true
	if err := cloud.Verify(ctx, dir, target); err != nil {
		verified = false
		resp.Warnings = append(resp.Warnings,
			"cloud verification failed, so nothing was evicted: "+err.Error())
	}

	if req.GetEvict() {
		if b.EvictAfterDays <= 0 {
			resp.Warnings = append(resp.Warnings,
				fmt.Sprintf("bucket %s has no eviction age set, so no local copy was released", b.Name))
		} else if verified {
			evicted, err := s.evictCold(ctx, dir, b, cloud, target)
			if err != nil {
				resp.Warnings = append(resp.Warnings, err.Error())
			}
			resp.Evicted = evicted
		}
	}

	after, _ := walkObjectKeys(dir)
	count, _, sizeErr := cloud.Size(ctx, target)
	if sizeErr != nil {
		// The cloud count is a recorded observation; failing to read it must not
		// fail a sync that actually moved the data.
		resp.Warnings = append(resp.Warnings, "could not read the cloud target's size: "+sizeErr.Error())
		count = -1
	}
	if beforeErr != nil {
		resp.Warnings = append(resp.Warnings, "could not read the cloud target's size before the sync, so the uploaded count is unknown: "+beforeErr.Error())
	}
	if beforeCloud >= 0 && count >= beforeCloud {
		resp.Uploaded = count - beforeCloud
	}
	s.mu.Lock()
	if cur, ok := s.buckets[b.Name]; ok {
		if count >= 0 {
			cur.CloudObjects = count
		}
		cur.LastSyncAt = s.now().UTC().Format(time.RFC3339)
		cur.SyncError = ""
		cur.LocalObjects = int64(len(after))
		b = cur
	}
	persistErr := s.persistLocked()
	s.mu.Unlock()
	if persistErr != nil {
		resp.Warnings = append(resp.Warnings, "could not persist the bucket's sync state: "+persistErr.Error())
	}
	resp.Bucket = b
	return resp, nil
}

// evictCold removes local copies older than the bucket's eviction age, one at a
// time and only after the whole-bucket check passed. It reports how many were
// released; a per-file failure is collected as a warning rather than aborting
// the sweep, so one unreadable file does not strand the rest.
func (s *server) evictCold(_ context.Context, dir string, b *onyxv1.Bucket, _ cloudTransport, _ string) (int64, error) {
	cutoff := s.now().Add(-time.Duration(b.EvictAfterDays) * 24 * time.Hour)
	keys, err := walkObjectKeys(dir)
	if err != nil {
		return 0, fmt.Errorf("list local objects for eviction: %w", err)
	}
	var evicted int64
	var failures []string
	for _, key := range keys {
		path := filepath.Join(dir, filepath.FromSlash(key))
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			failures = append(failures, key)
			continue
		}
		// The sidecar stays. Evicting the local copy does not delete the object
		// — it lives in the cloud, and the next read refetches it — so the
		// client metadata that goes with it has to survive too: a client that
		// PUT a document with its hash in `x-amz-meta-*` checks the hash on its
		// way back out, and dropping the sidecar here would make the refetched
		// object answer HEAD with no metadata at all. DeleteObject is what
		// removes it, because that is when the object genuinely stops existing.
		evicted++
	}
	if len(failures) > 0 {
		return evicted, fmt.Errorf("%d local copies could not be released: %s", len(failures), strings.Join(failures, ", "))
	}
	return evicted, nil
}

// recordSyncFailure stores why a sync failed so the bucket's state tells the
// operator, instead of only the caller of the failed request.
func (s *server) recordSyncFailure(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[name]; ok {
		b.SyncError = err.Error()
		_ = s.persistLocked()
	}
}

// remoteExists reports whether a remote name is in the rclone catalog. A
// transport that cannot answer the question (a test double, or a directory
// target) is treated as "yes" so only a definite miss is refused.
func (s *server) remoteExists(name string) bool {
	lister, ok := s.cloud.(remoteLister)
	if !ok {
		return true
	}
	remotes, known := lister.Remotes()
	if !known {
		return true
	}
	for _, r := range remotes {
		if r == name {
			return true
		}
	}
	return false
}

// errNotAnObject reports that a key resolves to something other than a regular
// file — which for this store means a directory, i.e. a key *prefix*.
//
// Objects are files under `<state-dir>/objects/<bucket>/`, so the key `postgres`
// is a directory exactly when objects were written under `postgres/…`, and S3
// has no object of that shape. Answering for one is not cosmetic: a key that is
// a prefix looks like a directory to a human and like a *file* to anything that
// only stats it, so a client probing with HEAD concluded the prefix was a single
// object and then could not list or age out what lived beneath it. Real S3
// answers 404 to such a key, and so does this store.
var errNotAnObject = errors.New("key resolves to a prefix, not an object")

// statObjectFile stats an object's path and refuses anything that is not a
// regular file. Every read of an object goes through it so HEAD and GET cannot
// disagree about whether a prefix exists: a HEAD answered from os.Stat alone
// would say 200 for a directory while the GET that followed failed to read it.
func statObjectFile(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errNotAnObject
	}
	return info, nil
}

// objectPathLocked validates the bucket/key pair and returns the on-disk
// path, refusing path traversal outside the object root.
func (s *server) objectPathLocked(bucket, key string) (string, error) {
	if _, ok := s.buckets[bucket]; !ok {
		return "", status.Error(codes.NotFound, "bucket not found")
	}
	if key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return "", status.Error(codes.InvalidArgument, "invalid key")
	}
	return filepath.Join(s.objects, bucket, filepath.Clean(key)), nil
}

func validBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func objectETag(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// cloudUpload writes one object to its exact place in the cloud target.
func cloudUpload(ctx context.Context, cloud cloudTransport, localPath, bucketTarget, key string) error {
	return cloud.Upload(ctx, localPath, bucketTarget+"/"+cloudPath(key))
}
