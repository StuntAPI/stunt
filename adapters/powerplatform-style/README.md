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
| GET | `/v2/environments/{env}/api/data/v9.2/accounts` | List Dataverse accounts (OData). |
| GET | `/v2/environments/{env}/connectors` | List connectors. |
| GET | `/v2/environments/{env}/flows` | List Power Automate flows. |
| POST | `/v2/environments/{env}/flows` | Create a flow. |

## Key shapes

- Environment: `{name, id, location, properties:{displayName, environmentSku, state}}`.
- OData response: `{value:[...]}`.
- Dataverse account: `{accountid, name, emailaddress1, telephone1, revenue, statecode, _primarycontactid_value}`.
- Flow: `{name, id, type:"Microsoft.Flow/flows", properties:{displayName, state}}`.

## Data model

Environments and Dataverse accounts are static (seeded). Flows are **stateful**:
created flows persist and appear in subsequent list requests.
