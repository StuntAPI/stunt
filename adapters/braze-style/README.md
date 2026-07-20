# braze-style

A stunt adapter simulating the **Braze REST API** (v2.0) for customer
engagement, for local testing.

## Simulated API

- **Name:** Braze REST API
- **Version:** `2.0`

## Why this adapter?

Braze (formerly Appboy) uses app-group API keys for auth and ingests user
data through batched `/users/track` calls (attributes, events, purchases).
Message sending supports multiple channels (push, email, etc.) in a single
call. This adapter lets you test the data ingestion and messaging flow locally.

## Auth

- **Bearer:** `Authorization: Bearer <app-group-api-key>`.
- **x-authorization:** `x-authorization: <app-group-api-key>` header also accepted.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/messages/send` | Send a message (`{messages:{email:{...}}, external_user_ids:[...]}`). |
| POST | `/users/track` | Ingest user data (`{attributes:[...], events:[...], purchases:[...]}`). |
| POST | `/users/alias/new` | Create a new user alias. |
| POST | `/users/identify` | Identify/merge a user. |
| POST | `/campaigns/trigger/send` | Trigger a campaign send. |
| GET | `/segments/list` | List segments. |
| GET | `/messages/scheduled` | List scheduled messages. |

## Key shapes

- Send response: `{message:"success", dispatch_id, recipients}`.
- Track response: `{message:"success", attributes_processed, events_processed, purchases_processed}`.
- Segments: `{message:"success", segments:[{id, name, status}]}`.

## Data model

Users are **stateful**. `/users/track` persists attributes that can be
looked up by external_id. Segments are static (seeded data).
