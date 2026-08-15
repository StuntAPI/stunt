# Amazon S3-style adapter

A stunt adapter for simulating **Amazon S3 (API version 2006-03-01)** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Amazon Web Services. "Amazon Web Services", "Amazon S3",
> and related marks are trademarks of their respective owners. See
> [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for **local
> development and testing only**.

## What it simulates

A faithful behavioral mock of Amazon S3's path-style API, designed for local
integration testing without a real AWS account:

- **Create bucket:** `PUT /{bucket}` → `200` (409 `BucketAlreadyOwnedByYou` if it exists).
- **Delete bucket:** `DELETE /{bucket}` → `204`. Per S3 semantics the bucket
  must be empty (otherwise 409 `BucketNotEmpty`); deleting a non-existent
  bucket is an idempotent no-op `204` to keep teardown/cleanup flows robust.
- **Upload object:** `PUT /{bucket}/{key}` (body = object content) → `200` with `ETag`.
- **Download object:** `GET /{bucket}/{key}` → `200` with raw body.
- **Object metadata:** `HEAD /{bucket}/{key}` → `200` with `Content-Length`, `ETag`, `Last-Modified`.
- **Delete object:** `DELETE /{bucket}/{key}` → `204` (idempotent).
- **ListObjectsV2:** `GET /{bucket}?list-type=2&max-keys=N&prefix=...` → **XML** `<ListBucketResult>`.
- **Bucket location:** `GET /{bucket}?location` → **XML** `<LocationConstraint>`.

Objects are **stateful**: an object uploaded via PUT appears in ListObjectsV2 for
the same bucket, enabling round-trip testing locally.

### Byte-exact binary round-trip

Object content is stored from the request's **verbatim bytes** (`raw_body`) in a
byte-exact blob store keyed by bucket/key; the collection holds metadata only.
Binary uploads (images, gzip streams, anything non-JSON) therefore round-trip
exactly — GET returns the same bytes, with the `Content-Type` captured from the
upload's `Content-Type` header (`application/octet-stream` by default).

### ListObjectsV2 pagination

ListObjectsV2 supports S3 cursor pagination:

- **Page size:** `max-keys` (S3 default `1000`; `0`/non-positive disables paging).
- **Cursor:** `continuation-token` — the opaque token echoed from the previous
  page's `<NextContinuationToken>`.
- When more pages remain, the response includes `<IsTruncated>true</IsTruncated>`
  and `<NextContinuationToken>`, plus `<KeyCount>` for `list-type=2` requests.

Prefix filtering (`prefix=...`) is applied **before** pagination, as in real S3.

ListObjectsV2 also honors the real S3 list params:

- **`start-after=...`** — list keys lexicographically after the given key
  (ListObjectsV2 only; echoed as `<StartAfter>`).
- **`delimiter=...`** — keys sharing the prefix up to and including the first
  delimiter occurrence roll up into `<CommonPrefixes>` entries (echoed as
  `<Delimiter>`).
- **`marker=...`** — ListObjects V1 (requests without `list-type=2`): list
  keys lexicographically after the given one (echoed as `<Marker>`).
- **`encoding-type=url`** — percent-encode keys, prefixes, delimiter,
  `start-after` and `marker` in the response (echoed as `<EncodingType>`).
  Any other value returns a `400 InvalidArgument` XML error, like real S3.
- **`fetch-owner=true`** — ListObjectsV2: include an `<Owner>` element in
  each `<Contents>` entry.

Keys and common prefixes are returned in ascending lexicographic order, and
all list filters are applied before pagination, as in real S3.

All XML responses use the correct S3 namespace:
`http://s3.amazonaws.com/doc/2006-03-01/`.

## Auth — AWS Signature Version 4 (SigV4), verified for real

Amazon S3 uses **AWS Signature Version 4** (SigV4) for authentication. This
adapter **recomputes the signature**: the canonical request is rebuilt from
the incoming request (method, RFC 3986-encoded path, sorted/encoded query,
signed headers, hashed payload), the string-to-sign is formed with the
`X-Amz-Date` header and the Credential scope, and the signing key
`HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request")`
is derived from the documented synthetic secret. A real SDK
(`aws-sdk-go`, `boto3`, ...) configured with the credentials below produces
signatures that verify against this adapter.

### Synthetic credentials (documented constants)

```
Access key ID:     AKIAIOSFODNN7EXAMPLE
Secret access key: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

These are the long-public example credentials from the AWS documentation —
no real account backs them. Configure your SDK's static-credential provider
with this pair and point the endpoint at the stunt service.

### Verification scheme

Header auth (`Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=<hex>`):

1. **Structure**: `Credential` (`<AK>/<YYYYMMDD>/<region>/s3/aws4_request`),
   `SignedHeaders`, and a hex `Signature` must be present.
2. **Access key**: must be the documented AKID, else `403 InvalidAccessKeyId`.
3. **Clock window**: `x-amz-date` is required and must be within ±15 minutes
   of the adapter clock (the engine's injectable clock), else
   `403 RequestTimeTooSkewed`.
4. **Payload hash**: the `x-amz-content-sha256` header (when present and not
   `UNSIGNED-PAYLOAD`/`STREAMING-*`) must equal `sha256` of the verbatim
   request bytes, else `400 XAmzContentSHA256Mismatch`; when absent the hash
   is computed from the raw body.
5. **Signature**: the recomputed hex signature (over the rebuilt canonical
   request, with the canonical headers taken from `SignedHeaders` — `host`
   resolves from the transport Host) must match, else
   `403 SignatureDoesNotMatch`.

Presigned URLs (`GET /{bucket}/{key}?X-Amz-Algorithm=...&X-Amz-Credential=...&X-Amz-Signature=...&X-Amz-Date=...&X-Amz-Expires=...`):

- Same credential/scope checks, plus `X-Amz-Expires` in `1..604800` and the
  expiry window `X-Amz-Date + X-Amz-Expires` checked against the clock
  (expired → `403 AccessDenied` "Request has expired").
- The canonical query for verification includes all query parameters
  **except** `X-Amz-Signature` itself; the payload hash defaults to
  `UNSIGNED-PAYLOAD` (an `X-Amz-Content-Sha256` query parameter overrides it).

### Known limitations

- The adapter sees the **decoded** path and query, so the canonical URI and
  query string are rebuilt by re-encoding the decoded values. Duplicate
  query keys and non-canonical encodings in the original wire request cannot
  be distinguished.
- The RFC 1123 `Date` header fallback is not parsed; `x-amz-date` is required.
- Interior whitespace collapsing in canonical header values is not applied.

### Auth errors

Requests without valid auth return S3-shaped **XML** errors:

```xml
<Error>
  <Code>SignatureDoesNotMatch</Code>
  <Message>The request signature we calculated does not match...</Message>
  <RequestId>...</RequestId>
</Error>
```

Missing `Credential`/`SignedHeaders`/`Signature` components yield `AccessDenied`;
a malformed credential yields `AuthorizationHeaderMalformed`; presigned URLs
with bad `X-Amz-*` parameters yield `AuthorizationQueryParametersError`; an
unknown access key yields `InvalidAccessKeyId`; a stale `x-amz-date` yields
`RequestTimeTooSkewed`; a lying `x-amz-content-sha256` yields `XAmzContentSHA256Mismatch`
(HTTP 400). Other auth failures return HTTP `403`.

### Clock-derived response data

- **ETag** is content-derived: the quoted SHA-256 hex digest of the object's
  verbatim bytes (real S3 uses the MD5 digest for non-multipart uploads; the
  engine's crypto module has no MD5, so the stronger digest is used — a
  documented deviation).
- **Last-Modified** (GET/HEAD headers, RFC 1123) and `<LastModified>` (XML,
  ISO 8601 with milliseconds) derive from the engine clock at upload time.
- Bucket `<CreationDate>` derives from the clock as well.

### Example

The signatures below are placeholders — compute real ones with your SDK, the
AWS CLI (`--endpoint-url`), or any SigV4 signer using the documented
credentials.

```bash
# Upload an object with SigV4 auth (signature computed by your signer)
curl -X PUT "http://localhost:PORT/mybucket/test.txt" \
  -H "Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=<computed>" \
  -H "x-amz-date: 20260120T000000Z" \
  -H "x-amz-content-sha256: <sha256-of-body>" \
  -H "Content-Type: text/plain" \
  -d '{"hello": "world"}'

# List objects
curl "http://localhost:PORT/mybucket?list-type=2"

# Without auth → 403 XML error
curl "http://localhost:PORT/mybucket?list-type=2"
# → <Error><Code>MissingSecurityHeader</Code>...</Error>

# Paginated listing
curl "http://localhost:PORT/mybucket?list-type=2&max-keys=10&continuation-token=<NextContinuationToken>"

# Binary round-trip (bytes stored verbatim, ETag = quoted sha256 of the bytes)
curl -X PUT "http://localhost:PORT/mybucket/photo.jpg" \
  -H "Authorization: AWS4-HMAC-SHA256 ..." \
  -H "Content-Type: image/jpeg" \
  --data-binary @photo.jpg
curl "http://localhost:PORT/mybucket/photo.jpg" -H "Authorization: ..." > out.jpg  # byte-identical
```

## Error responses

All errors use S3-shaped XML:

| Code | HTTP | When |
|------|------|------|
| `MissingSecurityHeader` | 403 | No `Authorization` header or presigned params |
| `AccessDenied` | 403 | SigV4 header missing `Credential`/`SignedHeaders`/`Signature`; expired presigned URL; missing/invalid `x-amz-date` |
| `AuthorizationHeaderMalformed` | 403 | Credential not `<AK>/YYYYMMDD/region/s3/aws4_request` |
| `AuthorizationQueryParametersError` | 403 | Invalid presigned `X-Amz-*` query params |
| `SignatureDoesNotMatch` | 403 | Recomputed signature does not match (wrong secret, tampered request, non-hex signature) |
| `InvalidAccessKeyId` | 403 | Access key is not the documented synthetic AKID |
| `RequestTimeTooSkewed` | 403 | `x-amz-date` outside the ±15-minute window |
| `XAmzContentSHA256Mismatch` | 400 | `x-amz-content-sha256` header does not match the body bytes |
| `InvalidArgument` | 400 | `encoding-type` other than `url` on a list request; invalid `x-amz-content-sha256` |
| `NoSuchBucket` | 404 | Bucket doesn't exist |
| `NoSuchKey` | 404 | Object key doesn't exist |
| `BucketAlreadyOwnedByYou` | 409 | Bucket already exists on PUT |
| `BucketNotEmpty` | 409 | `DELETE /{bucket}` on a bucket that still contains objects |

## API version

```
api:
  name: "Amazon S3 API"
  version: "2006-03-01"
```
