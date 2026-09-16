package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// newS3Handler serves the S3-compatible endpoint for storage.onyx.innotel.us
// (docs/design/11 §6.6). v0.1 implements the core object operations and
// authenticates with static credentials (S3_ACCESS_KEY/S3_SECRET_KEY).
//
// AWS SigV4 is verified in sigv4.go — both the `Authorization:
// AWS4-HMAC-SHA256` form every SDK produces and the presigned-url form in the
// query string, which is the only way a browser fetches an object directly.
// HTTP Basic is still accepted, because it is what this service accepted before
// SigV4 landed and dropping it would break a deployment on upgrade; see
// authenticateS3Request.
func newS3Handler(s *server) http.Handler {
	access := os.Getenv("S3_ACCESS_KEY")
	secret := os.Getenv("S3_SECRET_KEY")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if access != "" {
			if err := authenticateS3Request(r, access, secret, "s3"); err != nil {
				writeS3AuthError(w, err)
				return
			}
		}
		routeS3(s, w, r)
	})
}

// writeS3AuthError answers an authentication failure with the S3 error document
// the client expects: a `WWW-Authenticate` challenge for Basic, and the specific
// code (SignatureDoesNotMatch, RequestTimeTooSkewed, …) for SigV4, so a client
// can tell "wrong secret" from "your clock is wrong".
func writeS3AuthError(w http.ResponseWriter, err error) {
	if failure, ok := err.(*authError); ok {
		if failure.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `AWS4-HMAC-SHA256, Basic realm="onyx-objectstore"`)
		}
		writeS3Error(w, failure.status, failure.code, failure.message)
		return
	}
	writeS3Error(w, http.StatusForbidden, "AccessDenied", err.Error())
}

func routeS3(s *server, w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	switch {
	case len(parts) == 0:
		if r.Method == http.MethodGet {
			s.s3ListBuckets(w)
			return
		}
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported method on service root")
	case len(parts) == 1:
		s.s3Bucket(w, r, parts[0])
	default:
		s.s3Object(w, r, parts[0], strings.Join(parts[1:], "/"))
	}
}

func (s *server) s3ListBuckets(w http.ResponseWriter) {
	resp, err := s.ListBuckets(nil, &onyxv1.ListBucketsRequest{})
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	type b struct {
		Name         string `xml:"Name"`
		CreationDate string `xml:"CreationDate"`
	}
	xmlBuckets := struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		Xmlns   string   `xml:"xmlns,attr"`
		Buckets []b      `xml:"Buckets>Bucket"`
	}{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, bucket := range resp.Buckets {
		xmlBuckets.Buckets = append(xmlBuckets.Buckets, b{Name: bucket.Name, CreationDate: bucket.CreatedAt})
	}
	writeS3XML(w, http.StatusOK, xmlBuckets)
}

// bucketExists reports whether a bucket is registered. The S3 clients call
// HeadBucket before every upload, so a store that cannot answer it cannot be
// used by an SDK at all.
func (s *server) bucketExists(name string) bool {
	resp, err := s.ListBuckets(nil, &onyxv1.ListBucketsRequest{})
	if err != nil {
		return false
	}
	for _, b := range resp.Buckets {
		if b.Name == name {
			return true
		}
	}
	return false
}

// headStatus answers a HEAD with a status and no body — a HEAD response may not
// carry one, so the XML error document cannot be used here.
func headStatus(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(code)
}

func (s *server) s3Bucket(w http.ResponseWriter, r *http.Request, bucket string) {
	switch r.Method {
	case http.MethodHead:
		if s.bucketExists(bucket) {
			headStatus(w, http.StatusOK)
			return
		}
		headStatus(w, http.StatusNotFound)
	case http.MethodPut:
		if _, err := s.CreateBucket(nil, &onyxv1.CreateBucketRequest{Name: bucket}); err != nil {
			writeS3GRPCError(w, err)
			return
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		force := r.URL.Query().Get("force") == "1"
		if _, err := s.DeleteBucket(nil, &onyxv1.DeleteBucketRequest{Name: bucket, Force: force}); err != nil {
			writeS3GRPCError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		s.s3ListObjects(w, bucket)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported method")
	}
}

func (s *server) s3ListObjects(w http.ResponseWriter, bucket string) {
	s.mu.Lock()
	dir := filepath.Join(s.objects, bucket)
	entries, err := os.ReadDir(dir)
	s.mu.Unlock()
	if err != nil {
		if os.IsNotExist(err) {
			writeS3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket does not exist")
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	type c struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		Size         int64  `xml:"Size"`
	}
	xmlList := struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Xmlns       string   `xml:"xmlns,attr"`
		Name        string   `xml:"Name"`
		IsTruncated bool     `xml:"IsTruncated"`
		Contents    []c      `xml:"Contents"`
	}{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: bucket}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			keys = append(keys, e.Name())
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		info, err := os.Stat(filepath.Join(dir, k))
		if err != nil {
			continue
		}
		xmlList.Contents = append(xmlList.Contents, c{
			Key:          k,
			LastModified: info.ModTime().UTC().Format(time.RFC3339),
			Size:         info.Size(),
		})
	}
	writeS3XML(w, http.StatusOK, xmlList)
}

// userMetadata collects the `x-amz-meta-*` headers, keyed without the prefix —
// S3 stores and returns user metadata under the bare name, and returning it with
// the prefix would make a round trip through this store look different from a
// round trip through MinIO or S3 itself.
func userMetadata(h http.Header) map[string]string {
	meta := map[string]string{}
	for name, values := range h {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") && len(values) > 0 {
			meta[strings.TrimPrefix(lower, "x-amz-meta-")] = values[0]
		}
	}
	return meta
}

func writeUserMetadata(w http.ResponseWriter, meta map[string]string) {
	for name, value := range meta {
		w.Header().Set("x-amz-meta-"+name, value)
	}
}

func (s *server) s3Object(w http.ResponseWriter, r *http.Request, bucket, key string) {
	switch r.Method {
	case http.MethodHead:
		s.mu.Lock()
		path, err := s.objectPathLocked(bucket, key)
		var info os.FileInfo
		if err == nil {
			info, err = os.Stat(path)
		}
		s.mu.Unlock()
		if err != nil {
			if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") {
				headStatus(w, http.StatusNotFound)
				return
			}
			headStatus(w, http.StatusInternalServerError)
			return
		}
		if data, readErr := os.ReadFile(path); readErr == nil {
			sum := md5.Sum(data)
			w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
			w.Header().Set("Content-Type", http.DetectContentType(data))
		}
		w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
		writeUserMetadata(w, s.loadUserMeta(bucket, key))
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		w.WriteHeader(http.StatusOK)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "InvalidRequest", err.Error())
			return
		}
		meta, err := s.PutObject(nil, &onyxv1.PutObjectRequest{
			Bucket:      bucket,
			Key:         key,
			Data:        body,
			ContentType: r.Header.Get("Content-Type"),
		})
		if err != nil {
			writeS3GRPCError(w, err)
			return
		}
		if metaMap := userMetadata(r.Header); len(metaMap) > 0 {
			if err := s.saveUserMeta(bucket, key, metaMap); err != nil {
				writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
				return
			}
		}
		w.Header().Set("ETag", `"`+meta.Etag+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.mu.Lock()
		path, err := s.objectPathLocked(bucket, key)
		var data []byte
		if err == nil {
			data, err = os.ReadFile(path)
		}
		s.mu.Unlock()
		if err != nil {
			if os.IsNotExist(err) {
				writeS3Error(w, http.StatusNotFound, "NoSuchKey", "object does not exist")
				return
			}
			writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		w.Header().Set("Content-Type", http.DetectContentType(data))
		sum := md5.Sum(data)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		writeUserMetadata(w, s.loadUserMeta(bucket, key))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	case http.MethodDelete:
		if _, err := s.DeleteObject(nil, &onyxv1.DeleteObjectRequest{Bucket: bucket, Key: key}); err != nil {
			writeS3GRPCError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported method")
	}
}

func splitPath(p string) []string {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func writeS3XML(w http.ResponseWriter, code int, v any) {
	raw, err := xml.Marshal(v)
	if err != nil {
		writeS3Error(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(raw)
}

func writeS3Error(w http.ResponseWriter, code int, codeName, message string) {
	writeS3XML(w, code, struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: codeName, Message: message})
}

func writeS3GRPCError(w http.ResponseWriter, err error) {
	msg := err.Error()
	code := http.StatusInternalServerError
	codeName := "InternalError"
	switch {
	case strings.Contains(msg, "not found"):
		code, codeName = http.StatusNotFound, "NoSuchBucket"
	case strings.Contains(msg, "not empty"):
		code, codeName = http.StatusConflict, "BucketNotEmpty"
	case strings.Contains(msg, "exists"):
		code, codeName = http.StatusConflict, "BucketAlreadyExists"
	case strings.Contains(msg, "invalid"):
		code, codeName = http.StatusBadRequest, "InvalidArgument"
	}
	writeS3Error(w, code, codeName, msg)
}
