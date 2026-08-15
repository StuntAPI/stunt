# cloudkit-style

A stunt adapter simulating the **CloudKit Web Services API** with the
obscure server-to-server token auth model, for local testing.

## Simulated API

- **Name:** CloudKit Web Services API
- **Version:** `1`

## Why this adapter?

CloudKit Web Services uses a notoriously complex authentication scheme:
server-to-server request signing, which requires constructing a
string-to-sign from the request date, raw body, and path, then EC-Signing it
with a prime256v1 private key registered in the CloudKit dashboard. Getting
this auth right is one of the biggest pain points of CloudKit integration.
This adapter lets you test the record CRUD flow — with the signature
verified for real — without provisioning a CloudKit container.

## Auth (server-to-server request signature)

Every request must carry three headers:

```
X-Apple-CloudKit-Request-KeyID:           <key id of the server-to-server key>
X-Apple-CloudKit-Request-ISO8601Date:     <ISO 8601 / RFC3339 request date>
X-Apple-CloudKit-Request-SignatureBase64: <base64(ECDSA-SHA256(privkey, message))>
```

The string-to-sign is the colon join of the date header value, the
**verbatim raw request body bytes** (empty string when there is no body),
and the request path (plus `?k1=v1&k2=v2` with keys sorted when query
params are present — the adapter's documented canonical form):

```
message = ISO8601Date + ":" + raw_request_body + ":" + request_path
```

The signature is ECDSA over prime256v1 with SHA-256, serialized as the raw
64-byte `r||s` concatenation, base64-encoded. The request is rejected with
`401 {"serverErrorCode":"AUTHENTICATION_FAILED", ...}` when the KeyID is
unknown, the date is missing/malformed or more than **10 minutes** from the
server clock, or the signature does not verify under the registered key.

### Synthetic key material (documented constants)

The adapter verifies against this throwaway EC P-256 keypair (it exists
nowhere but this repository; public + low-entropy, local stunt only).
KeyID:

```
stunt-cloudkit-s2s-key-1
```

Private key (PKCS#8) — sign with this:

```
-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgYWuBd8XWfDZ/VcJu
QB09aJCel9cxSAjTK0x6bsCiCVGhRANCAAQ6HcT9YUUVXeqvZzOGGORZ89rQX0Ne
n8el83/HqrrAlhhMFWpHo3iuSuqqFdhgd9XBSPPM9+E2RK/+qy+C4Qiw
-----END PRIVATE KEY-----
```

Public key (the half the adapter verifies with):

```
-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEOh3E/WFFFV3qr2czhhjkWfPa0F9D
Xp/HpfN/x6q6wJYYTBVqR6N4rkrqqhXYYHfVwUjzzPfhNkSv/qsvguEIsA==
-----END PUBLIC KEY-----
```

Signing in Go:

```go
date := time.Now().UTC().Format(time.RFC3339)
msg := date + ":" + string(rawBody) + ":" + path // path: URL path, no query
h := sha256.Sum256([]byte(msg))
r, s, _ := ecdsa.Sign(rand.Reader, priv, h[:]) // priv: the key above
sig := make([]byte, 64)                        // raw r||s, 32 bytes each
r.FillBytes(sig[:32])
s.FillBytes(sig[32:])
req.Header.Set("X-Apple-CloudKit-Request-KeyID", "stunt-cloudkit-s2s-key-1")
req.Header.Set("X-Apple-CloudKit-Request-ISO8601Date", date)
req.Header.Set("X-Apple-CloudKit-Request-SignatureBase64",
    base64.StdEncoding.EncodeToString(sig))
```

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/database/1/{container}/{env}/public/records/lookup` | Look up records by name (`{records:[{recordName}]}`). |
| POST | `/database/1/{container}/{env}/public/records/modify` | Create/update/delete records (`{operations:[...]}`). |
| GET | `/database/1/{container}/{env}/public/records/query` | Query records (`{query:{recordType, filterBy:[{fieldName, comparator, fieldValue}], sortBy:[{fieldName, ascending}], ...}}`; comparators `EQUALS`, `NOT_EQUALS`, `LESS_THAN`, `GREATER_THAN`, `IN`, `BEGINS_WITH`, `CONTAINS`/`LIST_MEMBER`). |
| GET | `/database/1/{container}/{env}/public/users/current` | Get current user. |
| GET | `/database/1/{container}/{env}/public/zones/list` | List zones (body param: `zoneNamePrefix`). |

## Key shapes

- Record: `{recordName, recordType, fields:{<name>:{value}}, created:{timestamp, userRecordName, deviceID}, modified:{...}}`.
- Lookup response: `{records:[record, ...]}`.
- Query response: `{records:[record, ...]}`.
- Modify response: `{records:[record, ...]}`.
- Zone: `{zones:[{zoneName, zoneType}]}`.

## Data model

Records and zones are **stateful**. Two sample `Notes` records and default
zones are seeded on first access. Create/update/delete operations are
persistent within a test run.
