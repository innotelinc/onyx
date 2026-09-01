package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and ObjectStore (proto/onyx/v1/objectstore.proto).
// Bucket metadata persists as <state-dir>/buckets.json; objects live under
// <state-dir>/objects/<bucket>/<key>. v0.1 is the local engine; hybrid-cloud
// sync (CLOUD/TIERED tiers) lands with v0.4 (docs/design/11 §6.6).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedObjectStoreServer

	mu       sync.Mutex
	stateDir string
	objects  string
	buckets  map[string]*onyxv1.Bucket
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.ObjectStoreServer = (*server)(nil)

func newServer(stateDir string) (*server, error) {
	s := &server{
		stateDir: stateDir,
		objects:  filepath.Join(stateDir, "objects"),
		buckets:  map[string]*onyxv1.Bucket{},
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
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &onyxv1.ListBucketsResponse{Buckets: out}, nil
}

func (s *server) CreateBucket(_ context.Context, req *onyxv1.CreateBucketRequest) (*onyxv1.Bucket, error) {
	if !validBucketName(req.GetName()) {
		return nil, status.Error(codes.InvalidArgument, "invalid bucket name (3-63 chars, lowercase, dots/dashes)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[req.GetName()]; ok {
		return nil, status.Error(codes.AlreadyExists, "bucket exists")
	}
	tier := req.GetTier()
	if tier == onyxv1.BucketTier_BUCKET_TIER_UNSPECIFIED {
		tier = onyxv1.BucketTier_LOCAL
	}
	if tier != onyxv1.BucketTier_LOCAL && req.GetCloudTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "cloud_target is required for CLOUD/TIERED buckets")
	}
	b := &onyxv1.Bucket{
		Name:        req.GetName(),
		Tier:        tier,
		CloudTarget: req.GetCloudTarget(),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
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

func (s *server) DeleteBucket(_ context.Context, req *onyxv1.DeleteBucketRequest) (*onyxv1.DeleteBucketResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	dir := filepath.Join(s.objects, b.Name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if len(entries) > 0 && !req.GetForce() {
		return nil, status.Error(codes.FailedPrecondition, "bucket is not empty (use force)")
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	delete(s.buckets, b.Name)
	if err := s.persistLocked(); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &onyxv1.DeleteBucketResponse{Deleted: true}, nil
}

func (s *server) PutObject(_ context.Context, req *onyxv1.PutObjectRequest) (*onyxv1.ObjectMeta, error) {
	if req.GetBucket() == "" || req.GetKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket and key are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[req.GetBucket()]; !ok {
		return nil, status.Error(codes.NotFound, "bucket not found")
	}
	path, err := s.objectPathLocked(req.GetBucket(), req.GetKey())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := os.WriteFile(path, req.GetData(), 0o640); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	sum := md5.Sum(req.GetData())
	meta := &onyxv1.ObjectMeta{
		Bucket:       req.GetBucket(),
		Key:          req.GetKey(),
		SizeBytes:    int64(len(req.GetData())),
		Etag:         hex.EncodeToString(sum[:]),
		ContentType:  req.GetContentType(),
		LastModified: time.Now().UTC().Format(time.RFC3339),
	}
	return meta, nil
}

func (s *server) GetObject(_ context.Context, req *onyxv1.GetObjectRequest) (*onyxv1.GetObjectResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.objectPathLocked(req.GetBucket(), req.GetKey())
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Error(codes.NotFound, "object not found")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &onyxv1.GetObjectResponse{
		Meta: &onyxv1.ObjectMeta{
			Bucket:    req.GetBucket(),
			Key:       req.GetKey(),
			SizeBytes: int64(len(data)),
		},
		Data: data,
	}, nil
}

func (s *server) DeleteObject(_ context.Context, req *onyxv1.DeleteObjectRequest) (*onyxv1.DeleteObjectResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.objectPathLocked(req.GetBucket(), req.GetKey())
	if err != nil {
		return nil, err
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &onyxv1.DeleteObjectResponse{Deleted: err == nil}, nil
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
