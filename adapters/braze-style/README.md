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
call. This adapter lets you test the data ingestion, profile export, and
messaging flow locally.

## Auth

- **Bearer:** `Authorization: Bearer <app-group-api-key>`.
- **x-authorization:** `x-authorization: <app-group-api-key>` header also accepted.

The presented key is **validated** against the adapter's key store: only a
known, unexpired key is accepted (Braze app-group API keys do not expire;
stored entries carry a far-future expiry). The well-known test key
`test-app-group-api-key` is seeded automatically on first request. Any
other key — missing, unknown, or expired — receives:

```json
{"message": "Unauthorized. A valid API key is required."}
```

(with HTTP `401`.)

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/messages/send` | Send a message (`{messages:{email:{...}}, external_user_ids:[...]}`). |
| POST | `/messages/schedule/create` | Schedule a message send (`{schedule:{time}, ...}`). |
| GET | `/messages/scheduled` | List upcoming scheduled broadcasts (requires `end_time`). |
| POST | `/users/track` | Ingest user data (`{attributes:[...], events:[...], purchases:[...]}`). |
| POST | `/users/alias/new` | Create user aliases (new alias-only users or existing users). |
| POST | `/users/identify` | Merge alias-only profiles into identified profiles. |
| POST | `/users/export/ids` | Export profiles by `external_ids` / `user_aliases`. |
| POST | `/users/delete` | Hard-delete profiles (permanent, like the real API). |
| POST | `/campaigns/trigger/send` | Trigger an API-triggered campaign send. |
| GET | `/segments/list` | List segments. |
| POST | `/webhooks` | Register an outbound webhook target (local extension). |
| GET | `/webhooks` | List registered webhook targets. |

## Data model

Users are **stateful**. Everything ingested through `/users/track` is
persisted and comes back out through `/users/export/ids`:

- **Attributes** — reserved profile fields (`first_name`, `last_name`,
  `email`, `dob`, `home_city`, `country`, `language`, `phone`, `time_zone`,
  `gender`, `email_subscribe`, `push_subscribe`) land at the top level of
  the profile; every other key is stored as a **custom attribute**.
- **Events** — stored with the real event-object vocabulary
  (`name`, `time`, `properties`, `app_id`) and aggregated on export into
  `custom_events: [{name, first, last, count}]`.
- **Purchases** — stored with the real purchase-object vocabulary
  (`product_id`, `currency`, `price`, `quantity`, `time`, `properties`,
  `app_id`) and aggregated on export into `purchases: [{name, first, last,
  count}]` (name is the `product_id`; count is the number of tracked
  purchase records).

Records are addressed by any of the real identifiers — `external_id`,
`user_alias` (both create the profile when missing), or `braze_id` /
`email` / `phone` (resolve-only, like the real API). The real
`_update_existing_only` flag is honored: update-only records never create
users, and unknown targets are skipped without an error.

Backing collections: `users` (profiles), `events` (raw custom-event
records), `purchases` (raw purchase records), `campaigns` (seeded
API-triggered campaigns used for id validation), `dispatches` (one record
per send, keyed by the dispatch id), `schedules` (scheduled sends), and
`webhooks` (registered targets).

### Seeded campaigns

`cmp001` (Welcome Email) and `cmp002` (Weekly Newsletter) are seeded into
the campaigns store on first use. Each carries `channels` and
`message_variation_ids: ["variant-1", "variant-2"]`. Real Braze campaign
and message-variation ids are dashboard-minted UUIDs; these synthetic ids
keep the simulator deterministic. `/messages/send` and
`/campaigns/trigger/send` validate against this store and
`/messages/schedule/create` accepts them for `campaign_id`.

## Key shapes

- Track response:
  `{message:"success", attributes_processed, events_processed, purchases_processed}`
  plus an `errors` array when individual records were rejected (see below).
- Send responses: `{message:"success", dispatch_id}` (+
  `send_id` echo when provided). `dispatch_id` is a 32-char lowercase hex
  string, like the real API.
- Schedule create: `{message:"success", dispatch_id, schedule_id}`;
  `schedule_id` is UUID-shaped.
- Scheduled list: `{scheduled_broadcasts:[{name, id, type, tags,
  next_send_time, schedule_type}]}` — the real envelope for upcoming
  scheduled campaigns and Canvases.
- Export: `{message:"success", users:[...], invalid_user_ids:[...]}`; each
  user object carries `created_at`, `external_id`, `user_aliases`,
  `braze_id`, the reserved profile fields, `custom_attributes`,
  `custom_events`, and `purchases`. `fields_to_export` projects the
  profile (empty/omitted exports everything, the endpoint's real default).
- Delete: `{deleted: n}` — the real envelope (no `message` key).
- Alias/identify: `{aliases_processed: n, message:"success"}`.
- Segments: `{message:"success", segments:[{id, name, status}]}`.

### users/track per-record status

Braze's real response contract is partial success: `message` stays
`"success"`, processed counts reflect what was ingested, and rejected
records are listed in the `errors` array — each entry is
`{"<ERROR_NAME>": "<human-readable detail>"}` naming the input array and
index. Implemented checks (real vocabulary):

| Error | Rejected when |
|-------|---------------|
| `EMAIL_BAD_FORMAT` | attribute carries a malformed email |
| `EXTERNAL_USER_ID_TOO_LARGE` | `external_id` exceeds 987 bytes |
| `BAD_EMAIL_SUBSCRIPTION_STATE` / `BAD_PUSH_SUBSCRIPTION_STATE` | subscription state outside `subscribed`/`unsubscribed`/`opted_in` |
| `Invalid 'properties' field` | reserved keys used as property names (`time`, `event_name`, `product_id`, `quantity`, `price`, `currency`) |
| `BAD_REQUEST` (simulator detail) | record missing required fields (`name`/`time`/`product_id`/`currency`/`price`) or an identifier, non-object records, unparseable ISO 8601 `time`, non-ISO-4217 currency, or `quantity` outside 1..100 |

Whole-request fatal errors use Braze's fatal-error table verbatim, e.g.
`{"message":"Max Input Length Exceeded"}` (> 75 external ids in one
`/users/track` call), `{"message":"Invalid Campaign ID"}`,
`{"message":"Message Variant Unspecified"}`,
`{"message":"Invalid Message Variant"}`, `{"message":"No message to send"}`,
`{"message":"No Recipients"}`, `{"message":"The max number of
external_ids and aliases per request was exceeded"}` (> 50 ids on messaging
endpoints), `{"message":"The max number of ids per request was exceeded"}`
(> 50 ids on export/delete/alias/identify), and
`{"message":"Bad Request"}` (undecodable body, unparseable `send_at`).

### identify/alias semantics (real)

- `/users/alias/new` with an `external_id` that exists → the alias is
  appended to that profile's `user_aliases` (deduplicated; adding a pair
  that already exists is a no-op success).
- With an `external_id` that does **not** exist → the alias is added to no
  user (real behavior: nothing changes, still success).
- Without `external_id` → a new **alias-only** profile is created; track
  against it with `user_alias`.
- `/users/identify` with an alias-only profile and no user carrying the
  `external_id` → the profile is re-keyed: the `external_id` is added to
  the aliased user's record.
- `/users/identify` when the `external_id` user also exists → the
  alias-only profile is **merged into** the identified one (aliases,
  custom attributes, events, purchases) and the alias-only profile is
  removed.

## Scheduled messages (derive-on-read lifecycle)

`POST /messages/schedule/create` stores the schedule with the unix time it
should send at. `GET /messages/scheduled?end_time=<ISO 8601>` derives each
schedule's real state from the clock:

- before the scheduled time → still `scheduled`, listed as an upcoming
  broadcast (`name`, `id` = campaign id, `type: "Campaign"`, `tags`,
  `next_send_time`, `schedule_type` of `local_time_zones` /
  `intelligent_delivery` / the company time zone `UTC`);
- once the time passes → the first read marks it **sent**, records a
  dispatch (with the real 32-hex dispatch id minted at creation), and
  emits the `message.sent` webhook **exactly once** — the transition is
  persisted before the event is emitted. Sent broadcasts leave the
  upcoming list, like the real endpoint.

`end_time` is required (and must parse), matching the real endpoint.

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
| `message.sent` | `POST /messages/send` | `{dispatch_id, campaign_id, recipients, channels, timestamp}` |
| `message.sent` | a scheduled send transitions to sent (derive-on-read) | `{dispatch_id, campaign_id, schedule_id, recipients, channels, timestamp}` |
| `campaign.sent` | `POST /campaigns/trigger/send` | `{dispatch_id, campaign_id, recipients, channels, timestamp}` |

The `events` list at registration subscribes to event types (an empty list
subscribes to everything).

### Signature: unsigned by design

Braze's webhook channel sends arbitrary HTTP requests whose method,
headers, and body the **customer** configures per message; Braze applies no
provider-side signature (event analytics streaming goes through Braze
Currents, not webhooks). There is no documented outbound signature scheme to
reproduce, so this adapter emits **unsigned** deliveries with a synthetic
payload shape. Receivers needing authentication should treat these like a
Braze webhook configured with a customer-supplied `Authorization` header,
which by design this simulator does not add.
