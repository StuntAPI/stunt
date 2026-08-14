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
| POST | `/webhooks` | Register an outbound webhook target (local extension). |
| GET | `/webhooks` | List registered webhook targets. |

## Key shapes

- Send response: `{message:"success", dispatch_id, recipients}`.
- Track response: `{message:"success", attributes_processed, events_processed, purchases_processed}`.
- Segments: `{message:"success", segments:[{id, name, status}]}`.

## Data model

Users are **stateful**. `/users/track` persists attributes that can be
looked up by external_id. Segments are static (seeded data).

## Webhooks

Register an outbound webhook target (local extension — real Braze has no
webhook-registration REST API; webhooks are authored as campaign/canvas
messages in the dashboard):

```bash
curl -X POST "http://localhost:PORT/webhooks" \
  -H "Authorization: Bearer <app-group-api-key>" \
  -H "Content-Type: application/json" \
  -d '{"url": "http://localhost:9090/webhook", "events": ["message.sent"]}'
```

### Events emitted

| Event | Emitted when | Payload |
|-------|--------------|---------|
| `message.sent` | `POST /messages/send` | `{dispatch_id, recipients, channels, timestamp}` |
| `campaign.sent` | `POST /campaigns/trigger/send` | `{dispatch_id, campaign_id, timestamp}` |

The `events` list at registration subscribes to event types (an empty list
subscribes to everything).

### Signature: unsigned by design

Braze's webhook channel sends arbitrary HTTP requests whose method, headers,
and body the **customer** configures per message; Braze applies no
provider-side signature (event analytics streaming goes through Braze
Currents, not webhooks). There is no documented outbound signature scheme to
reproduce, so this adapter emits **unsigned** deliveries with a synthetic
payload shape. Receivers needing authentication should treat these like a
Braze webhook configured with a customer-supplied `Authorization` header,
which by design this simulator does not add.
