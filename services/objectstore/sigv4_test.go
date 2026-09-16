package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testAccessKey = "AKIDEXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	testRegion    = "us-east-1"
	testService   = "s3"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestDeriveSigningKeyPublishedVector pins the key derivation to the worked
// example published in the AWS documentation (secret wJalr… EXAMPLEKEY, date
// 20150830, region us-east-1, service iam). It is the anchor: everything else in
// this file uses this function, so if it is wrong, nothing else means anything.
func TestDeriveSigningKeyPublishedVector(t *testing.T) {
	got := hex.EncodeToString(deriveSigningKey(testSecretKey, "20150830", "us-east-1", "iam"))
	const want = "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got != want {
		t.Fatalf("deriveSigningKey = %s, want %s", got, want)
	}
}

// TestURIEncodeAWSRules covers the encoding rules the canonical request depends
// on: unreserved characters pass through, a space is %20 and never "+", and a
// path keeps its separators while a query value does not.
func TestURIEncodeAWSRules(t *testing.T) {
	cases := []struct {
		in       string
		slash    bool
		expected string
	}{
		{"simple", true, "simple"},
		{"a b", true, "a%20b"},
		{"a/b", true, "a%2Fb"},
		{"a/b", false, "a/b"},
		{"a+b", true, "a%2Bb"},
		{"~-._", true, "~-._"},
		{"é", true, "%C3%A9"},
		{"*'()", true, "%2A%27%28%29"},
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.slash); got != c.expected {
			t.Errorf("uriEncode(%q, slash=%v) = %q, want %q", c.in, c.slash, got, c.expected)
		}
	}
}

// signedHeader builds an `Authorization: AWS4-HMAC-SHA256 …` header for req the
// way a client would, so the verifier can be driven with a request that is
// genuinely signed rather than hand-written hex.
func signedHeader(t *testing.T, req *http.Request, signedHeaders []string, payloadHash string) string {
	t.Helper()

	auth := &sigV4Auth{
		accessKey:     testAccessKey,
		date:          req.Header.Get("X-Amz-Date")[:8],
		region:        testRegion,
		service:       testService,
		signedHeaders: signedHeaders,
	}
	query, err := canonicalQuery(req.URL, nil)
	if err != nil {
		t.Fatalf("canonicalQuery: %v", err)
	}
	canonical, err := canonicalRequest(req, signedHeaders, query, payloadHash)
	if err != nil {
		t.Fatalf("canonicalRequest: %v", err)
	}
	sum := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		req.Header.Get("X-Amz-Date"),
		auth.scope(auth.date),
		hex.EncodeToString(sum[:]),
	}, "\n")
	signature := signString(testSecretKey, auth.date, auth.region, auth.service, stringToSign)
	return sigV4Algorithm + " Credential=" + testAccessKey + "/" + auth.scope(auth.date) +
		", SignedHeaders=" + strings.Join(signedHeaders, ";") + ", Signature=" + signature
}

func newSignedRequest(t *testing.T, method, target string, body []byte, signedHeaders []string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Host = "storage.onyx.innotel.us"
	req.Header.Set("X-Amz-Date", time.Now().UTC().Format(amzDateFormat))
	req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))

	// A header cannot be signed unless it is present, so anything the caller
	// asks to sign is set here before the signature is computed — otherwise the
	// verifier refuses the request for the correct reason and the test would be
	// asserting against a malformed fixture.
	if contains(signedHeaders, "content-type") {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	if !contains(signedHeaders, "host") {
		signedHeaders = append([]string{"host"}, signedHeaders...)
	}
	if !contains(signedHeaders, "x-amz-content-sha256") {
		signedHeaders = append(signedHeaders, "x-amz-content-sha256")
	}
	if !contains(signedHeaders, "x-amz-date") {
		signedHeaders = append(signedHeaders, "x-amz-date")
	}
	req.Header.Set("Authorization", signedHeader(t, req, signedHeaders, sha256Hex(body)))
	return req
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestHeaderAuthAcceptsAProperlySignedRequest(t *testing.T) {
	body := []byte("hello onyx")
	req := newSignedRequest(t, http.MethodPut, "http://storage.onyx.innotel.us/bucket/key.txt", body, []string{"content-type"})

	if err := verifySigV4(req, testAccessKey, testSecretKey, testService); err != nil {
		t.Fatalf("a correctly signed request was refused: %v", err)
	}
	// Authentication must not consume the body: the handler still has to store it.
	got := make([]byte, len(body))
	if _, err := req.Body.Read(got); err != nil {
		t.Fatalf("reading the body after verification: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("body after verification = %q, want %q", got, body)
	}
}

func TestHeaderAuthRejectsTampering(t *testing.T) {
	body := []byte("hello onyx")

	t.Run("wrong secret", func(t *testing.T) {
		req := newSignedRequest(t, http.MethodPut, "http://h/bucket/key", body, nil)
		if err := verifySigV4(req, testAccessKey, "not-the-secret", testService); err == nil {
			t.Fatal("a signature made with another key was accepted")
		}
	})

	t.Run("unknown access key", func(t *testing.T) {
		req := newSignedRequest(t, http.MethodPut, "http://h/bucket/key", body, nil)
		if err := verifySigV4(req, "SOMEONE-ELSE", testSecretKey, testService); err == nil {
			t.Fatal("an unknown access key was accepted")
		}
	})

	t.Run("body changed after signing", func(t *testing.T) {
		req := newSignedRequest(t, http.MethodPut, "http://h/bucket/key", body, nil)
		req.Body = io.NopCloser(bytes.NewReader([]byte("malicious payload")))
		if err := verifySigV4(req, testAccessKey, testSecretKey, testService); err == nil {
			t.Fatal("a body that does not match X-Amz-Content-Sha256 was accepted")
		}
	})

	t.Run("path changed after signing", func(t *testing.T) {
		req := newSignedRequest(t, http.MethodPut, "http://h/bucket/key", body, nil)
		req.URL.Path = "/bucket/other-key"
		req.URL.RawPath = ""
		if err := verifySigV4(req, testAccessKey, testSecretKey, testService); err == nil {
			t.Fatal("a request whose path changed after signing was accepted")
		}
	})

	t.Run("a signed header removed", func(t *testing.T) {
		req := newSignedRequest(t, http.MethodPut, "http://h/bucket/key", body, []string{"content-type"})
		req.Header.Del("Content-Type")
		if err := verifySigV4(req, testAccessKey, testSecretKey, testService); err == nil {
			t.Fatal("a request missing a header it declared as signed was accepted")
		}
	})

	t.Run("wrong service in the scope", func(t *testing.T) {
		req := newSignedRequest(t, http.MethodPut, "http://h/bucket/key", body, nil)
		if err := verifySigV4(req, testAccessKey, testSecretKey, "sts"); err == nil {
			t.Fatal("a credential scoped to another service was accepted")
		}
	})

	t.Run("stale timestamp", func(t *testing.T) {
		req := newSignedRequest(t, http.MethodPut, "http://h/bucket/key", body, nil)
		stale := time.Now().UTC().Add(-2 * time.Hour)
		req.Header.Set("X-Amz-Date", stale.Format(amzDateFormat))
		req.Header.Set("Authorization", strings.Replace(
			req.Header.Get("Authorization"), time.Now().UTC().Format(shortDate), stale.Format(shortDate), 1))
		if err := verifySigV4(req, testAccessKey, testSecretKey, testService); err == nil {
			t.Fatal("a request dated outside the clock-skew window was accepted")
		}
	})
}

func TestPresignedRoundTripAndExpiry(t *testing.T) {
	key := "/bucket/report.pdf"
	amzDate := time.Now().UTC().Format(amzDateFormat)
	date := amzDate[:8]
	expires := "300"

	p := url.Values{}
	p.Set("X-Amz-Algorithm", sigV4Algorithm)
	p.Set("X-Amz-Credential", testAccessKey+"/"+date+"/"+testRegion+"/"+testService+"/aws4_request")
	p.Set("X-Amz-Date", amzDate)
	p.Set("X-Amz-Expires", expires)
	p.Set("X-Amz-SignedHeaders", "host")

	req := httptest.NewRequest(http.MethodGet, "http://storage.onyx.innotel.us"+key+"?"+p.Encode(), nil)
	req.Host = "storage.onyx.innotel.us"

	// Sign exactly as a client does: canonicalize everything except the
	// signature parameter, which cannot cover itself.
	query, err := canonicalQuery(req.URL, map[string]bool{"X-Amz-Signature": true})
	if err != nil {
		t.Fatalf("canonicalQuery: %v", err)
	}
	canonical, err := canonicalRequest(req, []string{"host"}, query, unsignedPayload)
	if err != nil {
		t.Fatalf("canonicalRequest: %v", err)
	}
	sum := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{
		sigV4Algorithm, amzDate, date + "/" + testRegion + "/" + testService + "/aws4_request",
		hex.EncodeToString(sum[:]),
	}, "\n")
	signature := signString(testSecretKey, date, testRegion, testService, stringToSign)

	p.Set("X-Amz-Signature", signature)
	signedURL := "http://storage.onyx.innotel.us" + key + "?" + p.Encode()

	t.Run("valid", func(t *testing.T) {
		fresh := httptest.NewRequest(http.MethodGet, signedURL, nil)
		fresh.Host = "storage.onyx.innotel.us"
		if err := verifySigV4(fresh, testAccessKey, testSecretKey, testService); err != nil {
			t.Fatalf("a valid presigned URL was refused: %v", err)
		}
	})

	t.Run("tampered key", func(t *testing.T) {
		evil := httptest.NewRequest(http.MethodGet, strings.Replace(signedURL, "report.pdf", "contract.pdf", 1), nil)
		evil.Host = "storage.onyx.innotel.us"
		if err := verifySigV4(evil, testAccessKey, testSecretKey, testService); err == nil {
			t.Fatal("a presigned URL for another key was accepted")
		}
	})

	t.Run("expired", func(t *testing.T) {
		old := url.Values{}
		for k, v := range p {
			old[k] = v
		}
		past := time.Now().UTC().Add(-2 * time.Hour)
		old.Set("X-Amz-Date", past.Format(amzDateFormat))
		expired := httptest.NewRequest(http.MethodGet, "http://storage.onyx.innotel.us"+key+"?"+old.Encode(), nil)
		expired.Host = "storage.onyx.innotel.us"
		err := verifySigV4(expired, testAccessKey, testSecretKey, testService)
		if err == nil {
			t.Fatal("an expired presigned URL was accepted")
		}
		if failure, ok := err.(*authError); !ok || failure.code != "AccessDenied" {
			t.Fatalf("expected AccessDenied for an expired URL, got %v", err)
		}
	})

	t.Run("dated in the future", func(t *testing.T) {
		soon := url.Values{}
		for k, v := range p {
			soon[k] = v
		}
		soon.Set("X-Amz-Date", time.Now().UTC().Add(time.Hour).Format(amzDateFormat))
		future := httptest.NewRequest(http.MethodGet, "http://storage.onyx.innotel.us"+key+"?"+soon.Encode(), nil)
		future.Host = "storage.onyx.innotel.us"
		if err := verifySigV4(future, testAccessKey, testSecretKey, testService); err == nil {
			t.Fatal("a presigned URL dated in the future was accepted")
		}
	})
}

// TestSignedRequestNeverFallsBackToBasic is the security property that matters
// most about keeping Basic for compatibility: a request that carries a SigV4
// `Authorization` header but a correct Basic *body* must not authenticate. If
// the two paths could both run, an attacker could sign with a key they hold and
// still be read as the Basic user.
func TestSignedRequestNeverFallsBackToBasic(t *testing.T) {
	body := []byte("x")
	req := newSignedRequest(t, http.MethodPut, "http://h/bucket/key", body, nil)
	req.Header.Set("Authorization", sigV4Algorithm+" Credential=WRONG/20250101/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=deadbeef")

	if err := authenticateS3Request(req, testAccessKey, testSecretKey, testService); err == nil {
		t.Fatal("a malformed SigV4 request was authenticated by the Basic path")
	}
}

func TestBasicAuthStillAccepted(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://h/bucket/key", nil)
	req.SetBasicAuth(testAccessKey, testSecretKey)
	if err := authenticateS3Request(req, testAccessKey, testSecretKey, testService); err != nil {
		t.Fatalf("Basic auth (the pre-SigV4 path) was refused: %v", err)
	}

	bad := httptest.NewRequest(http.MethodGet, "http://h/bucket/key", nil)
	bad.SetBasicAuth(testAccessKey, "wrong")
	if err := authenticateS3Request(bad, testAccessKey, testSecretKey, testService); err == nil {
		t.Fatal("Basic auth with the wrong password was accepted")
	}
}

func TestStreamingPayloadRefused(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "http://h/bucket/key", bytes.NewReader(nil))
	if err := verifyPayloadHash(req, streamingPayloadPrefix+"AWS4-HMAC-SHA256-PAYLOAD"); err == nil {
		t.Fatal("an unverifiable streaming payload was accepted")
	}
}


