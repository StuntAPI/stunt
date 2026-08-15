# Dropbox-style adapter

A stunt adapter for simulating a **Dropbox-style files API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Dropbox. "Dropbox" is a trademark of its respective
> owner. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for
> **local development and testing only**.

## What it simulates

A broader-than-minimal MVP of a Dropbox-style files API using the provider's
distinctive **RPC-style** endpoint pattern: every action is a
`POST /2/files/{action}` (or `/2/users/{action}`) with a JSON body. This is
intentionally different from REST-style adapters (e.g., `drive-style`) and
provides useful adapter-pattern variety for local testing.

Supported operations: file upload (with content), content download,
`list_folder` (path-prefix listing), `get_metadata`, `create_folder`,
`delete`, `get_temporary_link` (synthetic), and `get_current_account`.

State persists across requests: file content is stored in a filesystem-backed
blob store, and entry metadata in an SQLite-backed collection store. Data you
create in one request is visible in subsequent requests within the same
`stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/2/files/upload` | `files.star#on_upload` | Upload a file (JSON `{path, content}`) |
| POST | `/2/files/download` | `files.star#on_download` | Download file content (`{path}` or `{id}`) |
| POST | `/2/files/list_folder` | `files.star#on_list_folder` | List entries under a path prefix |
| POST | `/2/files/get_metadata` | `files.star#on_get_metadata` | Get entry metadata (`{path}`) |
| POST | `/2/files/create_folder` | `files.star#on_create_folder` | Create a folder (`{path}`) |
| POST | `/2/files/delete` | `files.star#on_delete` | Delete entry + content (`{path}`) |
| POST | `/2/files/get_temporary_link` | `files.star#on_get_temporary_link` | Synthetic temporary download link |
| POST | `/2/users/get_current_account` | `users.star#on_get_current_account` | Synthetic account info |

Any unmatched route returns `404 {"error":"resource_not_found"}`.

### Download format

`POST /2/files/download` returns the **raw file content** with
`Content-Type: application/octet-stream`. This mirrors the real provider's
behaviour of streaming binary content in the response body (without a JSON
envelope). The file metadata is available via `get_metadata`.

### Error format

Errors use a simplified Dropbox-style envelope with HTTP status `409`:

```json
{
  "error_summary": "path/not_found/..",
  "error": {".tag": "path/not_found"}
}
```

## Backing stores

| Store | Kind | Purpose |
|-------|------|---------|
| `entries` | collection | File/folder metadata records |
| `dropbox` | blob | File content (raw bytes) |
| `dropbox` | kv | Sequence counter for entry IDs |

IDs are generated with a synthetic `id_` prefix (e.g., `id_1`, `id_2`) via a
KV-backed sequence counter.

## Layout

```
adapter.yaml              Manifest: endpoints, resources, rules, identity
DISCLAIMER                Not affiliated / synthetic-only notice
README.md                 This file
scripts/
  files.star              File upload/download/list/metadata/folder/delete/temp-link handlers
  users.star              get_current_account handler
fixtures/
  entries.jsonl           Seed data for the entries collection
templates/
  file.json               Example file metadata response (faker placeholders)
schemas/
  file.schema.json        JSON Schema for a file/folder metadata object
```

## Auth

The adapter declares `identity.token_scheme: bearer` as metadata.

When an `Authorization: Bearer <token>` header IS sent, the token is now
**validated**: it must be registered in the KV store (`token_<token>` →
unix-seconds expiry) and unexpired. An unknown token returns `401` with
Dropbox's error envelope `{"error_summary": "invalid_access_token/..",
"error": {".tag": "invalid_access_token"}}` (an expired one returns the
`expired_access_token` variant). The static mock token `sl.test_token_mock`
is seeded automatically on first use with a far-future expiry.

Requests with NO `Authorization` header are still accepted — this preserves
the mock's original no-auth convenience for local testing (the shared
engine test helpers do not send one).

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  dropbox:
    adapter: ./adapters/dropbox-style
```

Then `stunt up` and make requests to the served address.

## Clock and content hashes

`server_modified` timestamps are **live**: `clock.now_rfc3339()` (RFC 3339
UTC) at request time. `client_modified` echoes the client-declared value
from the upload request (`Dropbox-API-Arg` header or JSON body) and
defaults to the upload time. Seeded entries are stamped once at seed time
and stay stable.

File metadata carries `content_hash` computed with Dropbox's own scheme
(the value behind the `Dropbox-API-Content-Hash` header of the real API):
the content is split into 4 MiB blocks, each block is SHA-256-hashed, the
raw 32-byte digests are concatenated, and that byte string is SHA-256-
hashed again, hex-encoded. The hash is content-derived — the same bytes
always hash identically regardless of id or upload order. Equivalent Go:

```go
func contentHash(b []byte) string {
	h := sha256.New()
	for i := 0; i == 0 || i < len(b); i += 4 << 20 {
		end := i + (4 << 20)
		if end > len(b) {
			end = len(b)
		}
		d := sha256.Sum256(b[i:end])
		h.Write(d[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
```
