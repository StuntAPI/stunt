# cloudkit-style

A stunt adapter simulating the **CloudKit Web Services API** with the
obscure server-to-server token auth model, for local testing.

## Simulated API

- **Name:** CloudKit Web Services API
- **Version:** `1`

## Why this adapter?

CloudKit Web Services uses a notoriously complex authentication scheme:
`X-Apple-CloudKit-Request`, which requires constructing a string-to-sign
from the request path, body, and current timestamp, then HMAC-signing it
with a server-to-server key. Getting this auth right is one of the biggest
pain points of CloudKit integration. This adapter lets you test the record
CRUD flow without provisioning a CloudKit container.

## Auth

- **X-Apple-CloudKit-Request:** header must be present and non-empty.
  The real API requires a signature over a string-to-sign; here we do
  structural validation only (header presence check).

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/database/1/{container}/{env}/public/records/lookup` | Look up records by name (`{records:[{recordName}]}`). |
| POST | `/database/1/{container}/{env}/public/records/modify` | Create/update/delete records (`{operations:[...]}`). |
| GET | `/database/1/{container}/{env}/public/records/query` | Query by recordType + filters. |
| GET | `/database/1/{container}/{env}/public/users/current` | Get current user. |
| GET | `/database/1/{container}/{env}/public/zones/list` | List zones. |

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
