# powerplatform-style

A stunt adapter simulating the **Microsoft Power Platform API** (v2) with
environments, Dataverse entities, and Power Automate flows, for local testing.

## Simulated API

- **Name:** Microsoft Power Platform API
- **Version:** `2`

## Why this adapter?

Power Platform's OData-style API returns entities in `{value:[...]}` envelopes
and nests configuration under `properties`. Dataverse (formerly Common Data
Service) uses CRM-style entity names (accounts, contacts, etc.) with GUID IDs
and special field naming conventions (`_primarycontactid_value`). This adapter
lets you test environment enumeration and Dataverse queries locally.

## Auth

- **Bearer:** `Authorization: Bearer <entra-token>`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v2/environments` | List environments (OData). |
| GET | `/v2/environments/{env}/api/data/v9.2/accounts` | List Dataverse accounts (OData; `$filter`, `$orderby`, `$skip`, `$select`, `$count`). |
| POST | `/v2/environments/{env}/api/data/v9.2/accounts` | Create an account (201 + `Location` + `OData-Version: 4.0`). |
| GET | `/v2/environments/{env}/api/data/v9.2/accounts({accountid})` | Retrieve a single account. |
| PATCH | `/v2/environments/{env}/api/data/v9.2/accounts({accountid})` | Update an account (204). |
| DELETE | `/v2/environments/{env}/api/data/v9.2/accounts({accountid})` | Delete an account (204). |
| GET | `/v2/environments/{env}/connectors` | List connectors. |
| GET | `/v2/environments/{env}/flows` | List Power Automate flows. |
| POST | `/v2/environments/{env}/flows` | Create a flow. |

All endpoints require the Bearer token; a missing token returns `401` with
`{error:{code:"Unauthorized", message:...}}`.

## Query options

- **`$select`** (account list): comma-separated field projection applied to each
  returned account.
- **`$count=true`** (account list): adds `@odata.count` with the total number of
  accounts (before paging).
- **`$top` / `$skipToken`** (all list endpoints): OData cursor pagination. Pass
  `$top` for the page size; the response includes `@odata.nextLink` (round-trips
  `$top`/`$skipToken`) when there is a further page. Paging is disabled when
  `$top` is missing or `<= 0` — the whole list is returned with no next link.

## Key shapes

- Environment: `{name, id, location, properties:{displayName, environmentSku, state}}`.
- OData response: `{value:[...]}` (plus `@odata.context` on the account list).
- Dataverse account: `{accountid, name, emailaddress1, telephone1, revenue, statecode, _primarycontactid_value}`.
- Flow: `{name, id, type:"Microsoft.Flow/flows", properties:{displayName, state}}`.
- Missing account: `404` with `{error:{code:"0x80040217", message:"account With Id = <id> Does Not Exist"}}`.

## Data model

Environments and connectors are static (seeded). Dataverse accounts and flows
are **stateful**: accounts are seeded once into a collection and then fully
CRUD-able (create persists and appears in subsequent list/retrieve requests;
updates and deletes are visible immediately). Creating an account without an
`accountid` auto-generates one (`acc-<n>`). Created flows persist per
environment; an environment with no stored flows falls back to a seeded
`seeded-flow-001` entry in list responses.
