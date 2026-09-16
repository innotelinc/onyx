# AWS Signature Version 4 test suite (vendored)

The published conformance data for SigV4, taken verbatim and carrying AWS's own
`LICENSE` (Apache-2.0) and `NOTICE`:

    AWS Signature Version 4 Test Suite
    Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.

Where it came from, recorded because the primary sources have moved: the suite
was published as `awslabs/aws-sig-v4-test-suite` and `aws-samples/aws-sig-v4-test-suite`,
both of which now return 404, and the documentation page that described it
(`docs.aws.amazon.com/general/latest/gr/signature-v4-test-suite.html`) now
redirects to the reference index. These files were taken from the republished
copy at `github.com/saibotsivad/aws-sig-v4-test-suite` under its `raw/` path,
which carries the AWS LICENSE and NOTICE unmodified. The `.creq` content is
therefore AWS's; the mirror only re-serializes the requests.

Vendoring it is the point: `go test` has to run offline, and this suite is the
one authority that can tell us our signature implementation is wrong. A round
trip between this package and a client that shares our misreading of the
specification passes and proves nothing.

## Layout

`<case>/<case>.{req,authz,creq}` — a raw HTTP request, the `Authorization`
header AWS generated for it, and the canonical request that signature was
computed over.

## What is included

A deliberately chosen subset, not the whole suite: the cases that exercise the
rules an S3 endpoint actually uses — header folding and ordering, query
ordering and encoding, unreserved characters, key trimming, the body hash, and
path handling. The streaming (`*streaming*`), presigned/post-vanilla-style
variants, and the `get-header-value-multiline` cases are omitted: the first is
refused by this service by design, and the rest add no rule we do not already
cover.

Two cases are documented rather than asserted directly, because S3 diverges from
or disagrees with the suite:

- **`normalize-path/get-slash-pointless-dot`** — the generic algorithm
  normalizes `/./example` to `/example`. S3 does **not** normalize URI paths,
  because those are different object keys and normalizing would silently merge
  them. `TestSigV4VectorS3PathDivergence` pins that difference.
- **`post-x-www-form-urlencoded`** — the suite's own `.creq` and `.authz`
  disagree: the canonical request lists `content-length`, the `Authorization`
  header signs only three headers. The canonical request is the documentation
  form over *every* header; the signature covers the declared subset.
  `TestSigV4VectorAuthzMaySignFewerHeaders` asserts both facts.

Auditing these vectors is what found two real bugs in this service: a request
carrying no `X-Amz-Content-Sha256` was being signed as `UNSIGNED-PAYLOAD`
instead of over its body hash (which also meant the body went unverified while
looking authenticated), and a client that signs `Content-Length` could never
verify, because Go moves that header onto the request and out of `Header`.
