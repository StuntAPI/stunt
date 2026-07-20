# azure-storage-style

Azure Blob Storage REST API simulator for local testing.

> **Not affiliated with Microsoft.** Synthetic data only. See [DISCLAIMER](DISCLAIMER).

## Why

SAS tokens and SharedKey signing are a top local-dev pain for Azure Storage.
Real code must sign requests with HMAC-SHA256 over a canonical string-to-sign,
or append SAS tokens with the right query parameters. This mock lets you test
the full blob CRUD lifecycle locally with any structurally-valid auth.

## API version

- **API**: Azure Storage Blob REST API
- **Version**: `2024-08-04` (passed via `x-ms-version` header)

## Auth

Accepts three auth schemes (structural validation only):

1. **SharedKey** — `Authorization: SharedKey <accountName>:<signature>`
   - The signature is an HMAC-SHA256 over a string-to-sign: `method + canonicalized-headers + canonicalized-resource`
   - The HMAC is **not recomputed** (documented stretch goal); any header with account + non-empty base64 signature is accepted.

2. **SAS token** — query params: `?sv=2024-08-04&ss=b&srt=co&sp=...&sig=<base64-hmac>&se=...&st=...`
   - Validates presence of `sv`, `sig`, and `se`.

3. **Bearer** — `Authorization: Bearer <token>` (Azure Entra ID / OAuth2)
   - Accepts any non-empty bearer token.

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
| PUT | `/{container}/{blob}?comp=properties` | Set blob properties. |
| GET | `/{container}/{blob}?comp=metadata` | Get blob metadata. |
| PUT | `/{container}/{blob}?comp=metadata` | Set blob metadata. |
| PUT | `/{container}/{blob}?comp=block` | Upload block. |
| GET | `/{container}/{blob}?comp=blocklist` | Get block list. |

## Response format

Listings use **XML** (`Content-Type: application/xml`) with the
`<EnumerationResults>` shape. Errors use `<?xml version="1.0"?><Error><Code/><Message/></Error>`.

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
