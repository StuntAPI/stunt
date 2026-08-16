# azure-storage-style

Azure Blob Storage REST API simulator for local testing.

> **Not affiliated with Microsoft.** Synthetic data only. See [DISCLAIMER](DISCLAIMER).

## Why

SAS tokens and SharedKey signing are a top local-dev pain for Azure Storage.
Real code must sign requests with HMAC-SHA256 over a canonical string-to-sign,
or append SAS tokens with the right query parameters. This mock lets you test
the full blob CRUD lifecycle locally with really-verified SharedKey auth
(documented synthetic key) or structurally-valid SAS/bearer tokens.

## API version

- **API**: Azure Storage Blob REST API
- **Version**: `2024-08-04` (passed via `x-ms-version` header)

## Auth

Accepts three auth schemes:

1. **SharedKey** — `Authorization: SharedKey <accountName>:<signature>` —
   **verified for real** (see [SharedKey verification](#sharedkey-verification)).

2. **SAS token** — query params: `?sv=2024-08-04&ss=b&srt=co&sp=...&sig=<base64-hmac>&se=...&st=...`
   - Validates presence of `sv`, `sig`, and `se` (structural check).

3. **Bearer** — `Authorization: Bearer <token>` (Azure Entra ID / OAuth2)
   - Accepts any non-empty bearer token.

## SharedKey verification

The SharedKey signature is recomputed and compared, using the real Azure
Storage string-to-sign (2015-02-21+ form):

```
VERB\n
Content-Encoding\n
Content-Language\n
Content-Length\n            (empty string when the request has no content)
Content-MD5\n
Content-Type\n
Date\n
If-Modified-Since\n
If-Match\n
If-None-Match\n
If-Unmodified-Since\n
Range\n
CanonicalizedHeaders       x-ms-* headers, lowercased, sorted, "name:value\n" each
CanonicalizedResource      /<account><path> then "\n<name>:<value>" per query
                           parameter, names sorted lexicographically
```

`signature = base64( HMAC-SHA256( base64decode(accountKey), stringToSign ) )`
and it is compared with the value after the colon in the Authorization
header. Any mismatch (or an unknown account) returns the real Azure error:
**403** with the XML envelope `<Error><Code>AuthenticationFailed</Code>…`.

### Synthetic credentials (documented)

| Account | Key (raw) | Key (base64 — what SharedKey uses) |
|---------|-----------|------------------------------------|
| `stuntstorage` | `stunt-local-storage-signing-key` | `c3R1bnQtbG9jYWwtc3RvcmFnZS1zaWduaW5nLWtleQ==` |

Tests and clients derive the same MACs from these constants (the adapter's
Go tests sign with `crypto/hmac` + `encoding/base64` in Go). The key table
lives in `scripts/lib.star` (`_SHARED_KEYS`); add rows there for more
synthetic accounts.

### Signing example (Go)

```go
sts := strings.Join([]string{"PUT", "", "", "", "", "application/json", "", "", "", "", "", ""}, "\n") + "\n" +
    "x-ms-blob-type:BlockBlob\nx-ms-date:<http-date>\n" +
    "/stuntstorage/mycontainer/report.json"
mac := hmac.New(sha256.New, []byte("stunt-local-storage-signing-key"))
mac.Write([]byte(sts))
auth := "SharedKey stuntstorage:" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
```

### Timestamps

`Last-Modified`, `x-ms-creation-time`, and `ETag` reflect the actual store:
timestamps come from the engine clock (`clock.now_rfc3339()` rendered as an
RFC 1123 HTTP date — no hardcoded dates), and ETags are minted per write.

## Endpoints

Path-style URLs: `/{container}/{blob}`.

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/?comp=list` | ListContainers (XML). **Stateful.** |
| PUT | `/{container}` | Create container (`x-ms-blob-public-access`). |
| GET | `/{container}?restype=container&comp=list` | ListBlobs (XML). **Stateful.** |
| HEAD | `/{container}` | Container metadata. |
| DELETE | `/{container}` | Delete container. |
| PUT | `/{container}/{blob}` | Upload BlockBlob (`x-ms-blob-type`). |
| GET | `/{container}/{blob}` | Download blob. |
| HEAD | `/{container}/{blob}` | Blob metadata (`x-ms-blob-type`, `Content-Length`, `ETag`). |
| DELETE | `/{container}/{blob}` | Delete blob. |
| PUT | `/{container}/{blob}?comp=properties` | Set blob properties (`x-ms-blob-content-type`). |
| GET | `/{container}/{blob}?comp=properties` | Get blob properties (headers). |
| GET | `/{container}/{blob}?comp=metadata` | Get blob metadata. |
| PUT | `/{container}/{blob}?comp=metadata` | Set blob metadata. |
| PUT | `/{container}/{blob}?comp=block&blockid=<base64>` | Put Block — stage an uncommitted block. |
| PUT | `/{container}/{blob}?comp=blocklist` | Put Block List — commit the blob from the listed blocks. |
| GET | `/{container}/{blob}?comp=blocklist&blocklisttype=all\|committed\|uncommitted` | Get Block List (XML). |

## Block staging (Put Block / Put Block List / Get Block List)

The real Azure block-blob assembly model, stateful and byte-exact:

1. **Put Block** (`PUT /{container}/{blob}?comp=block&blockid=<base64>`,
   body = block bytes) stages the block as *uncommitted*. Blocks may be
   staged **in any order**, and re-staging an id replaces its bytes. Each
   `201` carries a `Content-MD5` header (base64 SHA-256 of the block — the
   crypto module has no MD5, a documented deviation, the same trade the S3
   adapter makes for its ETags).
2. **Get Block List** (`GET ...?comp=blocklist&blocklisttype=...`) returns
   the `<CommittedBlocks>` (commit order) and `<UncommittedBlocks>`
   (staged, sorted by block id). `blocklisttype` is required — missing is
   `400 MissingRequiredQueryParameter`, anything but
   `committed|uncommitted|all` is `400 InvalidQueryParameterValue`. With no
   committed blob and no staged blocks it is `404 BlobNotFound`.
3. **Put Block List** (`PUT ...?comp=blocklist`, XML body) commits the
   blob: the listed blocks (`<Latest>`/`<Uncommitted>` staged ids,
   `<Committed>` previously committed ids) are concatenated **in list
   order** into the blob content. A listed id that was never staged is
   `400 InvalidBlockList`; an unparseable body is `400 InvalidXMLDocument`.
   Committing consumes the staged blocks (unlisted ones are discarded,
   like the real service) and records the committed list — visible via
   Get Block List and byte-exact on `GET /{container}/{blob}`.

Block ids follow the real rules: non-empty base64, at most 64 decoded
bytes (`400 InvalidQueryParameterValue` otherwise). Deleting a blob (or
its container) discards its staged blocks. A single-shot `PUT` blob has
no addressable committed blocks — its committed list is empty.

## Response format

Listings use **XML** (`Content-Type: application/xml`) with the
`<EnumerationResults>` shape. Errors use `<?xml version="1.0"?><Error><Code/><Message/></Error>`.

## Pagination

ListContainers (`GET /?comp=list`) and ListBlobs (`GET /{container}?restype=container&comp=list`)
support Azure Storage's marker-based paging:

- `maxresults` — page size.
- `marker` — continuation token from the previous response.

Responses carry a `<NextMarker>` element; it is empty (`<NextMarker />`) on the
last page. `prefix` filtering on ListBlobs is applied before paging.

## Binary content (byte-exact round-trip)

Blob uploads are stored **verbatim**: the raw request bytes (`raw_body`) go into
a byte-exact blob store, and `GET /{container}/{blob}` returns those identical
bytes. Binary payloads (images, gzip, protobuf — any `Content-Type`) round-trip
losslessly, and `Content-Length`/`<ContentLength>` reflect the stored byte count.
Re-uploading an existing blob name overwrites it in place (same backing blob).

## Statefulness notes

- Creating a container that already exists updates it in place (201).
- `DELETE /{container}` also deletes all blobs in that container (202).
- `DELETE /{container}/{blob}` removes both the record and the stored bytes (202).

## Example

```
PUT /mycontainer/report.json
Authorization: SharedKey stuntstorage:uZ8...base64...==
x-ms-blob-type: BlockBlob

→ 201 Created

GET /mycontainer?restype=container&comp=list
Authorization: SharedKey stuntstorage:uZ8...base64...==

→ 200 application/xml
<EnumerationResults>
  <Blobs>
    <Blob>
      <Name>report.json</Name>
      <Properties>
        <BlobType>BlockBlob</BlobType>
        <ContentLength>42</ContentLength>
        ...
      </Properties>
    </Blob>
  </Blobs>
</EnumerationResults>

# SAS token query form also works:
GET /mycontainer/report.json?sv=2024-08-04&ss=b&srt=co&sp=r&sig=abc&se=2025-01-01T00:00:00Z
```

Block assembly example (ids are base64; order comes from the LIST):

```
PUT /mycontainer/report.bin?comp=block&blockid=YmxvY2stQQ==
Authorization: SharedKey stuntstorage:...
<body>                          → 201, Content-MD5: <base64 sha256 of block>

PUT /mycontainer/report.bin?comp=block&blockid=YmxvY2stQg==
Authorization: SharedKey stuntstorage:...
<body>                          → 201

GET /mycontainer/report.bin?comp=blocklist&blocklisttype=all
Authorization: SharedKey stuntstorage:...
→ 200 <BlockList><CommittedBlocks /><UncommittedBlocks>
        <Block><Name>YmxvY2stQQ==</Name><Size>5120</Size></Block>
        <Block><Name>YmxvY2stQg==</Name><Size>4096</Size></Block>
      </UncommittedBlocks></BlockList>

PUT /mycontainer/report.bin?comp=blocklist
Authorization: SharedKey stuntstorage:...
<?xml version="1.0" encoding="utf-8"?><BlockList>
  <Latest>YmxvY2stQQ==</Latest><Latest>YmxvY2stQg==</Latest>
</BlockList>                    → 201, ETag + Content-MD5 of the assembled bytes

GET /mycontainer/report.bin     → 200, block A bytes followed by block B bytes
```
