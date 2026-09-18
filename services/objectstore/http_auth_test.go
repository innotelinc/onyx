package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// An endpoint with no credentials must close, not open.
//
// The compose deployment publishes this port (2090) for the ingress, and an
// empty S3_ACCESS_KEY used to mean "authenticate nobody": every bucket was
// readable and writable by anyone who could reach the port. The refusal is
// tested here because it is the whole difference between a closed endpoint and
// an open one, and nothing else in the suite would notice it regressing.
func TestS3EndpointWithoutCredentialsRefusesAnonymousAccess(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY", "")
	t.Setenv("S3_SECRET_KEY", "")

	s := &server{objects: t.TempDir()}
	handler := newS3Handler(s, false)

	for _, target := range []string{"http://storage/", "http://storage/docs", "http://storage/docs/a.txt"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != 403 {
			t.Errorf("GET %s: status = %d, want 403 (body: %s)", target, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "AccessDenied") {
			t.Errorf("GET %s: expected the S3 AccessDenied document, got %s", target, rec.Body.String())
		}
	}
}

// The explicit opt-in is what a development run uses; it must actually route.
func TestS3EndpointAnonymousOptInStillServes(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY", "")
	t.Setenv("S3_SECRET_KEY", "")

	s := &server{objects: t.TempDir()}
	handler := newS3Handler(s, true)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://storage/", nil))
	if rec.Code != 200 {
		t.Fatalf("anonymous listing with the opt-in: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

// With credentials configured the endpoint authenticates as before: an unsigned
// request is refused, and the refusal is the S3 auth document rather than the
// unconfigured-credential one.
func TestS3EndpointWithCredentialsRequiresASignature(t *testing.T) {
	t.Setenv("S3_ACCESS_KEY", "onyx-access-key")
	t.Setenv("S3_SECRET_KEY", "secret")

	s := &server{objects: t.TempDir()}
	handler := newS3Handler(s, false)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://storage/docs", nil))
	if rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("unsigned request: status = %d, want an auth failure (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AccessDenied") {
		t.Fatalf("expected an S3 auth error document, got %s", rec.Body.String())
	}
}
