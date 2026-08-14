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

All XML responses use the correct S3 namespace:
`http://s3.amazonaws.com/doc/2006-03-01/`.

## Auth — AWS Signature Version 4 (SigV4)

Amazon S3 uses **AWS Signature Version 4** (SigV4) for authentication. This
adapter **validates** the SigV4 scheme structurally:

### SigV4 header validation

The `Authorization` header must have this format:

```
Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=<hex>
```

What is validated:

1. **Algorithm**: header must start with `AWS4-HMAC-SHA256 `.
2. **Credential**: must be `<AccessKey>/<YYYYMMDD>/<region>/s3/aws4_request`.
3. **SignedHeaders**: must be present.
4. **Signature**: must be a non-empty hex string.

What is NOT validated (stretch goal):

- The actual HMAC-SHA256 signature is not recomputed (would require the secret
  access key). Any well-formed SigV4 header is accepted.

### Presigned URL validation

Presigned GETs are validated by checking the `X-Amz-*` query parameters:

```
GET /{bucket}/{key}?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=...&X-Amz-Signature=...&X-Amz-Date=...
```

- `X-Amz-Algorithm` must be `AWS4-HMAC-SHA256`.
- `X-Amz-Credential` must be present.
- `X-Amz-Signature` must be present.

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
with bad `X-Amz-*` parameters yield `AuthorizationQueryParametersError`. All
auth failures return HTTP `403`.

### Example

```bash
# Upload an object with SigV4 auth
curl -X PUT "http://localhost:PORT/mybucket/test.txt" \
  -H "Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=fe5f80f77d5fa3beca038a248ff027d044aca418" \
  -H "x-amz-date: 20260120T000000Z" \
  -H "Content-Type: text/plain" \
  -d '{"hello": "world"}'

# List objects
curl "http://localhost:PORT/mybucket?list-type=2"

# Download
curl "http://localhost:PORT/mybucket/test.txt" \
  -H "Authorization: AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=fe5f80f77d5fa3beca038a248ff027d044aca418"

# Without auth → 403 XML error
curl "http://localhost:PORT/mybucket?list-type=2"
# → <Error><Code>MissingSecurityHeader</Code>...</Error>

# Paginated listing
curl "http://localhost:PORT/mybucket?list-type=2&max-keys=10&continuation-token=<NextContinuationToken>"

# Binary round-trip (bytes stored verbatim)
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
| `AccessDenied` | 403 | SigV4 header missing `Credential`/`SignedHeaders`/`Signature` |
| `AuthorizationHeaderMalformed` | 403 | Credential not `<AK>/YYYYMMDD/region/s3/aws4_request` |
| `AuthorizationQueryParametersError` | 403 | Invalid presigned `X-Amz-*` query params |
| `SignatureDoesNotMatch` | 403 | Malformed SigV4 algorithm or non-hex signature |
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
