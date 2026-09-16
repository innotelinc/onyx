package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4 request verification.
//
// Why this exists: the S3 data plane is not a REST API you can authenticate
// however you like. Every S3 SDK signs with SigV4 — there is no Basic-auth mode
// in the protocol — and a *presigned URL* is SigV4 in the query string, which is
// the only way a browser can fetch an object directly without the application
// proxying every byte. Until this landed, the object store accepted HTTP Basic
// only, so no SDK could talk to it and presigning was impossible. That is the
// blocker recorded in signara/docs/Roadmap.md §W3; it is now closed.
//
// Both shapes are verified here:
//
//	Authorization: AWS4-HMAC-SHA256 Credential=…/20150830/us-east-1/s3/aws4_request,
//	               SignedHeaders=host;x-amz-date, Signature=…
//	GET /bucket/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&…&X-Amz-Signature=…
//
// The algorithm is implemented from the specification rather than by importing
// an SDK, so the store has no dependency on one — and so a client's signature is
// checked against the same rules it was produced by rather than against ours.
// sigv4_test.go pins the signing-key derivation to a published vector and drives
// the full round trip through a real net/http request.

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"

	// A presigned request carries this because the body is not known at signing
	// time, and a browser GET has none.
	unsignedPayload = "UNSIGNED-PAYLOAD"

	// Streaming/chunked signing (`AWS4-HMAC-SHA256-PAYLOAD`) signs each chunk
	// separately. Nothing in this estate uses it, and accepting the request
	// without verifying the chunks would be worse than refusing it.
	streamingPayloadPrefix = "STREAMING-"

	// How far a signed request's timestamp may be from now. AWS uses 15 minutes;
	// it bounds how long a captured `Authorization` header stays usable.
	maxClockSkew = 15 * time.Minute

	// Bodies are buffered only to verify `x-amz-content-sha256`, so the ceiling
	// is what the process is willing to hold. Past it the request is refused
	// rather than streamed unverified.
	maxSignedBody = 64 << 20

	amzDateFormat = "20060102T150405Z"
	shortDate     = "20060102"
)

// authError is a failure that maps onto an S3 error document.
type authError struct {
	status  int
	code    string
	message string
}

func (e *authError) Error() string { return e.code + ": " + e.message }

func authFail(status int, code, message string) *authError {
	return &authError{status: status, code: code, message: message}
}

// sigV4Auth is the parsed credential material from an Authorization header.
type sigV4Auth struct {
	accessKey     string
	date          string
	region        string
	service       string
	signedHeaders []string
	signature     string
}

func (a *sigV4Auth) scope(date string) string {
	return date + "/" + a.region + "/" + a.service + "/aws4_request"
}

// parseAuthHeader reads `AWS4-HMAC-SHA256 Credential=…, SignedHeaders=…, Signature=…`.
func parseAuthHeader(header string) (*sigV4Auth, error) {
	if !strings.HasPrefix(header, sigV4Algorithm) {
		return nil, authFail(http.StatusUnauthorized, "AccessDenied", "not an AWS4-HMAC-SHA256 authorization")
	}
	rest := strings.TrimSpace(strings.TrimPrefix(header, sigV4Algorithm))
	fields := map[string]string{}
	for _, part := range strings.Split(rest, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, authFail(http.StatusBadRequest, "AuthorizationHeaderMalformed", "malformed component "+strconv.Quote(part))
		}
		fields[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}

	credential := fields["Credential"]
	signed := fields["SignedHeaders"]
	signature := fields["Signature"]
	if credential == "" || signed == "" || signature == "" {
		return nil, authFail(http.StatusBadRequest, "AuthorizationHeaderMalformed", "Credential, SignedHeaders and Signature are all required")
	}

	// Credential is `<access-key>/<date>/<region>/<service>/aws4_request`, and the
	// access key itself may contain no slash, so the scope is the last four.
	parts := strings.Split(credential, "/")
	if len(parts) < 5 {
		return nil, authFail(http.StatusBadRequest, "AuthorizationHeaderMalformed", "Credential is not <key>/<date>/<region>/<service>/aws4_request")
	}
	tail := parts[len(parts)-4:]
	if tail[3] != "aws4_request" {
		return nil, authFail(http.StatusBadRequest, "AuthorizationHeaderMalformed", "the credential scope must terminate in aws4_request")
	}
	return &sigV4Auth{
		accessKey:     strings.Join(parts[:len(parts)-4], "/"),
		date:          tail[0],
		region:        tail[1],
		service:       tail[2],
		signedHeaders: splitSignedHeaders(signed),
		signature:     signature,
	}, nil
}

func splitSignedHeaders(value string) []string {
	out := make([]string, 0, 8)
	for _, h := range strings.Split(value, ";") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

// looksSigned reports whether the request claims SigV4, in either shape. Used to
// decide between SigV4 and the legacy Basic path, so that a request which *tries*
// to sign and gets it wrong is refused rather than falling back to Basic.
func looksSigned(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), sigV4Algorithm) {
		return true
	}
	return r.URL.Query().Get("X-Amz-Signature") != ""
}

// authenticateS3Request accepts a SigV4 request (header or presigned) or, for
// backwards compatibility with what v0.1 shipped, HTTP Basic.
func authenticateS3Request(r *http.Request, accessKey, secretKey, service string) error {
	if looksSigned(r) {
		return verifySigV4(r, accessKey, secretKey, service)
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != accessKey || pass != secretKey {
		return authFail(http.StatusUnauthorized, "AccessDenied", "bad credentials")
	}
	return nil
}

// verifySigV4 checks a signed request and leaves r.Body intact for the handler.
func verifySigV4(r *http.Request, accessKey, secretKey, service string) error {
	if r.Header.Get("Authorization") != "" {
		return verifyHeaderAuth(r, accessKey, secretKey, service)
	}
	return verifyPresigned(r, accessKey, secretKey, service)
}

func verifyHeaderAuth(r *http.Request, accessKey, secretKey, service string) error {
	auth, err := parseAuthHeader(r.Header.Get("Authorization"))
	if err != nil {
		return err
	}
	if auth.accessKey != accessKey {
		return authFail(http.StatusForbidden, "InvalidAccessKeyId", "unknown access key")
	}
	if auth.service != service {
		return authFail(http.StatusBadRequest, "AuthorizationHeaderMalformed", "credential scope names service "+strconv.Quote(auth.service))
	}

	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return authFail(http.StatusBadRequest, "AccessDenied", "X-Amz-Date is required for AWS4-HMAC-SHA256 requests")
	}
	if err := checkTimestamp(amzDate); err != nil {
		return err
	}
	// The date in the credential scope must be the date in X-Amz-Date, or the
	// signing key is derived over the wrong day.
	if scopeDate := amzDate; len(amzDate) >= 8 {
		if scopeDate[:8] != auth.date {
			return authFail(http.StatusBadRequest, "AuthorizationHeaderMalformed", "X-Amz-Date and the credential scope disagree on the date")
		}
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}
	if err := verifyPayloadHash(r, payloadHash); err != nil {
		return err
	}

	query, err := canonicalQuery(r.URL, nil)
	if err != nil {
		return err
	}
	canonical, err := canonicalRequest(r, auth.signedHeaders, query, payloadHash)
	if err != nil {
		return err
	}
	return compareSignature(r, auth, secretKey, canonical)
}

func verifyPresigned(r *http.Request, accessKey, secretKey, service string) error {
	query := r.URL.Query()
	if got := query.Get("X-Amz-Algorithm"); got != sigV4Algorithm {
		return authFail(http.StatusBadRequest, "AuthorizationQueryParametersError", "X-Amz-Algorithm must be "+sigV4Algorithm)
	}
	credential := query.Get("X-Amz-Credential")
	signed := query.Get("X-Amz-SignedHeaders")
	signature := query.Get("X-Amz-Signature")
	amzDate := query.Get("X-Amz-Date")
	if credential == "" || signed == "" || signature == "" || amzDate == "" {
		return authFail(http.StatusBadRequest, "AuthorizationQueryParametersError", "X-Amz-Credential, X-Amz-Date, X-Amz-SignedHeaders and X-Amz-Signature are all required")
	}

	parts := strings.Split(credential, "/")
	if len(parts) < 5 || parts[len(parts)-1] != "aws4_request" {
		return authFail(http.StatusBadRequest, "AuthorizationQueryParametersError", "X-Amz-Credential is not <key>/<date>/<region>/<service>/aws4_request")
	}
	tail := parts[len(parts)-4:]
	auth := &sigV4Auth{
		accessKey:     strings.Join(parts[:len(parts)-4], "/"),
		date:          tail[0],
		region:        tail[1],
		service:       tail[2],
		signedHeaders: splitSignedHeaders(signed),
		signature:     signature,
	}
	if auth.accessKey != accessKey {
		return authFail(http.StatusForbidden, "InvalidAccessKeyId", "unknown access key")
	}
	if auth.service != service {
		return authFail(http.StatusBadRequest, "AuthorizationQueryParametersError", "the credential scope names service "+strconv.Quote(auth.service))
	}

	// A presigned URL is a bearer token: anyone holding it can fetch the object
	// until it expires, so the window is enforced here and not left to the client.
	expires, err := strconv.ParseInt(query.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires < 0 {
		return authFail(http.StatusBadRequest, "AuthorizationQueryParametersError", "X-Amz-Expires must be a non-negative number of seconds")
	}
	if expires > 7*24*3600 {
		return authFail(http.StatusBadRequest, "AuthorizationQueryParametersError", "X-Amz-Expires may not exceed 604800 seconds")
	}
	signedAt, perr := time.Parse(amzDateFormat, amzDate)
	if perr != nil {
		return authFail(http.StatusBadRequest, "AuthorizationQueryParametersError", "X-Amz-Date is not an ISO8601 basic timestamp")
	}
	now := time.Now().UTC()
	if now.After(signedAt.Add(time.Duration(expires) * time.Second)) {
		return authFail(http.StatusForbidden, "AccessDenied", "the presigned URL has expired")
	}
	// Not only the end of the window: a URL dated in the future would otherwise
	// be usable for its full lifetime from a clock that is simply wrong.
	if signedAt.After(now.Add(maxClockSkew)) {
		return authFail(http.StatusForbidden, "AccessDenied", "the presigned URL is dated in the future")
	}

	// The signature cannot cover itself.
	canonicalQueryString, qerr := canonicalQuery(r.URL, map[string]bool{"X-Amz-Signature": true})
	if qerr != nil {
		return qerr
	}
	canonical, err := canonicalRequest(r, auth.signedHeaders, canonicalQueryString, unsignedPayload)
	if err != nil {
		return err
	}
	return compareSignature(r, auth, secretKey, canonical)
}

func compareSignature(r *http.Request, auth *sigV4Auth, secretKey, canonical string) error {
	// `X-Amz-Date` is what the string-to-sign uses for header auth; for a
	// presigned URL it is the query parameter of the same name.
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = r.URL.Query().Get("X-Amz-Date")
	}

	sum := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		auth.scope(auth.date),
		hex.EncodeToString(sum[:]),
	}, "\n")

	expected := signString(secretKey, auth.date, auth.region, auth.service, stringToSign)
	if !hmac.Equal([]byte(strings.ToLower(auth.signature)), []byte(expected)) {
		return authFail(http.StatusForbidden, "SignatureDoesNotMatch", "the request signature we calculated does not match the signature you provided")
	}
	return nil
}

// signString produces the hex HMAC signature for a string-to-sign.
func signString(secretKey, date, region, service, stringToSign string) string {
	mac := hmac.New(sha256.New, deriveSigningKey(secretKey, date, region, service))
	mac.Write([]byte(stringToSign))
	return hex.EncodeToString(mac.Sum(nil))
}

// deriveSigningKey is the v4 key derivation: HMAC chain over date → region →
// service → aws4_request, seeded with "AWS4"+secret.
func deriveSigningKey(secretKey, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secretKey), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// checkTimestamp refuses a signed request whose clock is outside the skew window.
func checkTimestamp(amzDate string) error {
	t, err := time.Parse(amzDateFormat, amzDate)
	if err != nil {
		return authFail(http.StatusBadRequest, "AccessDenied", "X-Amz-Date is not an ISO8601 basic timestamp")
	}
	if diff := time.Since(t); diff > maxClockSkew || diff < -maxClockSkew {
		return authFail(http.StatusForbidden, "RequestTimeTooSkewed", "the difference between the request time and the current time is too large")
	}
	return nil
}

// verifyPayloadHash checks the declared body hash, then restores the body so the
// handler can read it after authentication.
func verifyPayloadHash(r *http.Request, expected string) error {
	if expected == unsignedPayload {
		return nil
	}
	if strings.HasPrefix(expected, streamingPayloadPrefix) {
		return authFail(http.StatusNotImplemented, "NotImplemented", "streaming SigV4 payloads are not supported")
	}
	if r.Body == nil {
		// An empty body still has a hash, and it is the one clients send for GET.
		if emptyHash := sha256.Sum256(nil); hex.EncodeToString(emptyHash[:]) == expected {
			return nil
		}
		return authFail(http.StatusForbidden, "SignatureDoesNotMatch", "the body hash does not match X-Amz-Content-Sha256")
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxSignedBody+1))
	if err != nil {
		return authFail(http.StatusBadRequest, "InvalidRequest", "could not read the request body to verify its hash")
	}
	if int64(len(body)) > maxSignedBody {
		return authFail(http.StatusRequestEntityTooLarge, "EntityTooLarge", "the body is too large to verify against X-Amz-Content-Sha256")
	}
	// Restore it: authentication is a precondition of handling, not a consumer.
	r.Body = io.NopCloser(bytes.NewReader(body))

	sum := sha256.Sum256(body)
	if !hmac.Equal([]byte(hex.EncodeToString(sum[:])), []byte(strings.ToLower(expected))) {
		return authFail(http.StatusForbidden, "SignatureDoesNotMatch", "the body hash does not match X-Amz-Content-Sha256")
	}
	return nil
}

// canonicalRequest assembles the six required lines. See AWS "Create a canonical
// request" — the order is fixed and each line is significant, including the
// trailing newline after the canonical headers.
func canonicalRequest(r *http.Request, signedHeaders []string, query, payloadHash string) (string, error) {
	headers, list, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}
	uri := r.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	return strings.Join([]string{
		r.Method,
		uri,
		query,
		headers,
		list,
		payloadHash,
	}, "\n"), nil
}

// canonicalHeaders renders the signed headers in the lowercase, sorted form the
// client signed, and returns the header list for the canonical request.
func canonicalHeaders(r *http.Request, signedHeaders []string) (string, string, error) {
	if len(signedHeaders) == 0 {
		return "", "", authFail(http.StatusBadRequest, "AuthorizationHeaderMalformed", "SignedHeaders is empty")
	}
	names := append([]string(nil), signedHeaders...)
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		var value string
		if name == "host" {
			// Go promotes the Host header onto the request, out of Header.
			value = r.Host
		} else {
			values, ok := r.Header[http.CanonicalHeaderKey(name)]
			if !ok {
				// Every header named as signed must be present: otherwise a
				// client could sign a header, drop it in transit, and the
				// signature would still verify over nothing.
				return "", "", authFail(http.StatusForbidden, "SignatureDoesNotMatch", "a header named in SignedHeaders is absent: "+name)
			}
			value = strings.Join(values, ",")
		}
		b.WriteString(name)
		b.WriteString(":")
		b.WriteString(collapseSpaces(value))
		b.WriteString("\n")
	}
	return b.String(), strings.Join(names, ";"), nil
}

// canonicalQuery renders the query string the way the signature covers it:
// params sorted by name, values sorted within a name, each side URI-encoded.
// `skip` names parameters excluded from the signature (only X-Amz-Signature).
func canonicalQuery(u *url.URL, skip map[string]bool) (string, error) {
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", authFail(http.StatusBadRequest, "InvalidRequest", "the query string could not be parsed")
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			pairs = append(pairs, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(pairs, "&"), nil
}

// uriEncode implements the AWS URI-encoding rules: unreserved characters pass
// through, everything else becomes %XX with uppercase hex. `encodeSlash` is false
// for a path, where the separators carry meaning.
func uriEncode(value string, encodeSlash bool) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case strings.IndexByte(unreserved, c) >= 0:
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// collapseSpaces trims a header value and folds internal runs of spaces, which
// is the form the signature covers.
func collapseSpaces(value string) string {
	fields := strings.Fields(value)
	return strings.Join(fields, " ")
}
