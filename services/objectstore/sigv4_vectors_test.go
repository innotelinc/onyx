package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Replay of the published AWS Signature Version 4 test suite, vendored verbatim
// under testdata/sigv4 (with its LICENSE and NOTICE). Each case is a raw HTTP
// request, the Authorization header AWS generated for it, and the canonical
// request that signature was computed over.
//
// Why vectors rather than more round-trip tests: sigv4_test.go already signs a
// request with this package and verifies it with this package, which proves the
// two halves agree with *each other* and nothing else. A shared misreading of the
// specification — the wrong payload hash, an unsorted query, a wrongly folded
// header — round-trips perfectly and still rejects every real SDK. Replaying the
// specification's own conformance data is what pins the algorithm to the
// published rules. The SDK interop check against the running store is the other
// half of the argument; this file is the half that can run in CI.
const (
	vectorAccessKey = "AKIDEXAMPLE"
	vectorSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorService   = "service"
)

// vectorClock is the instant every vendored vector is dated at. The suite was
// published in 2015, so without a pinned clock the skew check would refuse all of
// them and the test would be asserting the wrong failure.
var vectorClock = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

var vectorCases = []struct {
	name string
	// s3Divergence marks a case where S3 deliberately departs from the generic
	// algorithm; asserted by TestSigV4VectorS3PathDivergence instead.
	s3Divergence bool
}{
	{name: "get-vanilla"},
	{name: "get-unreserved"},
	{name: "get-header-value-trim"},
	{name: "get-header-key-duplicate"},
	{name: "get-vanilla-query-order-key-case"},
	{name: "get-vanilla-query-order-value"},
	{name: "get-vanilla-query-unreserved"},
	{name: "post-header-key-sort"},
	{name: "post-vanilla-empty-query-value"},
	{name: "post-x-www-form-urlencoded"},
	{name: "normalize-path/get-space"},
	{name: "normalize-path/get-slash-pointless-dot", s3Divergence: true},
}

// s3Vector is one case as it sits on disk.
type s3Vector struct {
	method  string
	target  string
	headers [][2]string // ordered, duplicates preserved
	body    string
	authz   string
	creq    string
}

// headerNames is the request's own header list, lowercased, deduplicated and
// sorted. This — not the credential's SignedHeaders — is the set the published
// canonical request is computed over, and the two are not always the same: see
// TestSigV4VectorAuthzMaySignFewerHeaders.
func (v s3Vector) headerNames() []string {
	seen := make(map[string]bool, len(v.headers))
	names := make([]string, 0, len(v.headers))
	for _, h := range v.headers {
		name := strings.ToLower(h[0])
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func loadVector(t *testing.T, name string) s3Vector {
	t.Helper()
	// Vendored as <suite>/<case>/<case>.<ext>, the layout upstream uses.
	base := filepath.Join("testdata", "sigv4", name, filepath.Base(name))

	raw, err := os.ReadFile(base + ".req")
	if err != nil {
		t.Fatalf("reading the vector request: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	// The request line can itself contain a space — the `get-space` vector sends
	// a raw one — so the target is whatever sits between method and version
	// rather than a whitespace-delimited field.
	requestLine := strings.TrimSuffix(strings.TrimRight(lines[0], "\r"), "\n")
	first, last := strings.IndexByte(requestLine, ' '), strings.LastIndexByte(requestLine, ' ')
	if first < 0 || last <= first {
		t.Fatalf("malformed request line %q", lines[0])
	}
	if version := requestLine[last+1:]; version != "HTTP/1.1" {
		t.Fatalf("unexpected protocol in request line %q", requestLine)
	}
	v := s3Vector{method: requestLine[:first], target: requestLine[first+1 : last]}

	// Headers run to the blank line; anything after it is the body. The last
	// header of several vectors has no trailing newline at all.
	i := 1
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			i++
			break
		}
		field, value, ok := strings.Cut(lines[i], ":")
		if !ok {
			t.Fatalf("malformed header line %q", lines[i])
		}
		// The value is kept byte-for-byte, leading space included: the suite has
		// a case whose entire point is that the client signs it with that space.
		v.headers = append(v.headers, [2]string{field, value})
	}
	if i < len(lines) {
		v.body = strings.TrimSuffix(strings.Join(lines[i:], "\n"), "\n")
	}

	v.authz = strings.TrimSpace(readVectorFile(t, base+".authz"))
	// The canonical request never ends in a newline — its last line is the
	// payload hash — so trailing newlines in the file are noise.
	v.creq = strings.TrimRight(readVectorFile(t, base+".creq"), "\r\n")
	return v
}

func readVectorFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// buildVectorRequest reconstructs the request as a Go server would receive it.
func buildVectorRequest(t *testing.T, v s3Vector) *http.Request {
	t.Helper()
	var body io.Reader
	if v.body != "" {
		body = strings.NewReader(v.body)
	}
	// A raw space is not valid in a request line, so the `get-space` vector is
	// re-encoded the way a client actually sends it.
	target := strings.ReplaceAll(v.target, " ", "%20")
	req := httptest.NewRequest(v.method, "http://example.amazonaws.com"+target, body)
	for _, h := range v.headers {
		switch strings.ToLower(h[0]) {
		case "host":
			req.Host = h[1]
		case "content-length":
			// Setting the header directly would be invisible: Go moves
			// Content-Length onto the request and drops it from Header. The
			// value is already right, having been derived from the body above.
			continue
		default:
			req.Header.Add(h[0], h[1])
		}
	}
	req.Header.Set("Authorization", v.authz)
	return req
}

func TestSigV4PublishedVectors(t *testing.T) {
	for _, tc := range vectorCases {
		t.Run(tc.name, func(t *testing.T) {
			v := loadVector(t, tc.name)
			req := buildVectorRequest(t, v)

			got := canonicalRequestFor(t, req, v.headerNames())
			if tc.s3Divergence {
				// Asserted by TestSigV4VectorS3PathDivergence; a match here would
				// mean the deviation is gone and that test's premise is stale.
				if got == v.creq {
					t.Fatalf("this vector's canonical request now matches the generic algorithm")
				}
				return
			}
			if got != v.creq {
				t.Errorf("canonical request differs from the published vector\n── got ──\n%s\n── want ──\n%s",
					showEmptyLines(got), showEmptyLines(v.creq))
			}

			// The canonical request matching is necessary but not sufficient:
			// the string-to-sign, key derivation and signature comparison are
			// three more places to be wrong, and the suite's Authorization
			// header is what actually exercises them.
			verifier := &sigV4Verifier{
				accessKey: vectorAccessKey,
				secretKey: vectorSecretKey,
				service:   vectorService,
				now:       clockAt(vectorClock),
			}
			if err := verifier.verify(req); err != nil {
				t.Errorf("the vector's own signature was rejected: %v", err)
			}
		})
	}
}

// TestSigV4VectorS3PathDivergence pins a deliberate departure from the generic
// algorithm, so that it is a decision rather than an accident.
//
// The generic rule normalizes the URI path, collapsing `/./`, and the suite's
// `get-slash-pointless-dot` vector therefore signs `GET /./example` as
// `GET /example`. S3 does not: AWS states that URI paths are not normalized for
// requests to Amazon S3, because `/./photo` and `/photo` are *different object
// keys* and normalizing would silently merge two distinct objects — one of which
// a client could then overwrite by accident.
//
// Asserting the mismatch is the whole value: the test fails if normalization is
// ever introduced, and it fails if a re-vendored suite stops carrying the case.
// It also confines the difference to the path line, so a second, unintended
// divergence cannot hide behind this one.
func TestSigV4VectorS3PathDivergence(t *testing.T) {
	const name = "normalize-path/get-slash-pointless-dot"
	v := loadVector(t, name)
	req := buildVectorRequest(t, v)

	if path := req.URL.EscapedPath(); path != "/./example" {
		t.Fatalf("the vector no longer carries an unnormalized path: %q", path)
	}

	got := strings.Split(canonicalRequestFor(t, req, v.headerNames()), "\n")
	want := strings.Split(v.creq, "\n")
	if len(got) != len(want) {
		t.Fatalf("canonical requests have different shapes: %d lines vs %d", len(got), len(want))
	}
	if got[1] != "/./example" {
		t.Errorf("the store signed a path it should have left alone: %q", got[1])
	}
	if want[1] != "/example" {
		t.Errorf("the generic algorithm is expected to normalize the path; the vector says %q", want[1])
	}
	got[1], want[1] = "", ""
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("more than the path line differs from the vector:\n── got ──\n%s\n── want ──\n%s",
			showEmptyLines(strings.Join(got, "\n")), showEmptyLines(strings.Join(want, "\n")))
	}
}

// TestSigV4VectorAuthzMaySignFewerHeaders covers a case where the suite's own two
// files disagree, which is worth pinning rather than papering over.
//
// `post-x-www-form-urlencoded` sends Content-Length and Content-Type, and its
// published canonical request lists all four headers — content-length included —
// while its Authorization header signs only three. The canonical request is the
// *documentation* form, computed over every header the request carries; the
// signature covers only the subset the client declared.
//
// That distinction is the store's behaviour too, and it is the safe one: a
// signature is checked over what the client says it signed, so a header the
// client deliberately left out of the signature cannot be used to alter the
// request. Both halves are asserted — the full-header canonical request matches
// the published vector, and the signature over the declared subset verifies.
func TestSigV4VectorAuthzMaySignFewerHeaders(t *testing.T) {
	const name = "post-x-www-form-urlencoded"
	v := loadVector(t, name)
	req := buildVectorRequest(t, v)

	auth, err := parseAuthHeader(v.authz)
	if err != nil {
		t.Fatalf("parsing the vector's Authorization header: %v", err)
	}
	full := v.headerNames()
	if len(auth.signedHeaders) >= len(full) {
		t.Fatalf("this vector no longer signs a subset (%d declared vs %d present)", len(auth.signedHeaders), len(full))
	}
	if !contains(full, "content-length") {
		t.Fatalf("the vector no longer carries content-length, so it tests nothing here")
	}
	if contains(auth.signedHeaders, "content-length") {
		t.Fatalf("the vector now signs content-length, so the two files no longer disagree")
	}

	if got := canonicalRequestFor(t, req, full); got != v.creq {
		t.Errorf("canonical request over every header differs from the vector\n── got ──\n%s\n── want ──\n%s",
			showEmptyLines(got), showEmptyLines(v.creq))
	}
	if got := canonicalRequestFor(t, req, auth.signedHeaders); got == v.creq {
		t.Errorf("the declared-subset canonical request unexpectedly matches the full-header one")
	}

	verifier := &sigV4Verifier{
		accessKey: vectorAccessKey,
		secretKey: vectorSecretKey,
		service:   vectorService,
		now:       clockAt(vectorClock),
	}
	if err := verifier.verify(req); err != nil {
		t.Errorf("the vector's signature over its declared headers was rejected: %v", err)
	}
}

// canonicalRequestFor derives the canonical request over the given header set.
func canonicalRequestFor(t *testing.T, req *http.Request, signedHeaders []string) string {
	t.Helper()
	query, err := canonicalQuery(req.URL, nil)
	if err != nil {
		t.Fatalf("canonicalQuery: %v", err)
	}
	payloadHash, err := (&sigV4Verifier{}).payloadHash(req)
	if err != nil {
		t.Fatalf("payloadHash: %v", err)
	}
	canonical, err := canonicalRequest(req, signedHeaders, query, payloadHash)
	if err != nil {
		t.Fatalf("canonicalRequest: %v", err)
	}
	return canonical
}

func clockAt(t time.Time) func() time.Time { return func() time.Time { return t } }

// showEmptyLines makes the blank lines in a canonical request visible, since
// several of them are significant and a diff would otherwise be unreadable.
func showEmptyLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = "(blank)"
		}
	}
	return strings.Join(lines, "\n")
}
