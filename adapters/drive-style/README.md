# Google-Drive-style adapter

A stunt adapter for simulating a **Google-Drive-style files API** locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Google / Google Drive. "Google" and "Google Drive" are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A broader-than-minimal MVP of a Google-Drive-style files API: file upload
(with content), file metadata retrieval, content download (`alt=media`),
file listing, file metadata update (patch/rename/trash), file deletion,
folder creation, storage quota (`about`), and a minimal `changes` endpoint.

State persists across requests: file content is stored in a filesystem-backed
blob store, and file metadata in an SQLite-backed collection store. Data you
create in one request is visible in subsequent requests within the same
`stunt up` session.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/upload/drive/v3/files` | `files.star#on_upload` | Upload a file (JSON `{name, content}` or folder via `mimeType`) |
| GET | `/drive/v3/files/{id}` | `files.star#on_get` | Retrieve file metadata |
| GET | `/drive/v3/files/{id}?alt=media` | `files.star#on_get` | Download file content |
| GET | `/drive/v3/files` | `files.star#on_list` | List all non-trashed files |
| PATCH | `/drive/v3/files/{id}` | `files.star#on_patch` | Update file metadata (name, trashed, …) |
| DELETE | `/drive/v3/files/{id}` | `files.star#on_delete` | Permanently delete a file |
| GET | `/drive/v3/about` | `misc.star#on_about` | Return synthetic storage quota + user |
| GET | `/drive/v3/changes` | `misc.star#on_changes` | Return a minimal (empty) change list |

Any unmatched route returns `404 {"error":"resource_not_found"}`.

## Backing stores

| Store | Kind | Purpose |
|-------|------|---------|
| `files` | collection | File/folder metadata records |
| `drive` | blob | File content (raw bytes) |
| `drive` | kv | Sequence counter for file IDs |

IDs are generated with a provider-style prefix (`file_`) via a KV-backed
sequence counter.

## Layout

```
adapter.yaml              Manifest: endpoints, resources, rules, identity
DISCLAIMER                Not affiliated / synthetic-only notice
README.md                 This file
scripts/
  files.star              File upload/get/list/patch/delete handlers
  misc.star               About (quota) + changes handlers
fixtures/
  files.jsonl             Seed data for the files collection
templates/
  file.json               Example file response (faker placeholders)
schemas/
  file.schema.json        JSON Schema for a file object
```

## Auth

The adapter declares `identity.token_scheme: bearer` as metadata. Auth is **not
enforced** — any (or no) `Authorization` header is accepted. This is intentional
for local testing convenience.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  drive:
    adapter: ./adapters/drive-style
```

Then `stunt up` and make requests to the served address.
