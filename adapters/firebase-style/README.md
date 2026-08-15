# Firebase-style adapter

A stunt adapter for simulating the **Firebase Auth + Firestore + Cloud Messaging (FCM)**
APIs locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by Google / Firebase. "Firebase" and related marks are trademarks of their
> respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for
> **local development and testing only**.

## What it simulates

Three Firebase surfaces with their distinctive shapes:

### Auth (Identity Toolkit)

- **v1 REST:** `POST /v1/accounts:signInWithPassword`, `POST /v1/accounts:signUp`,
  `POST /v1/accounts:signInWithIdp`, `POST /v1/accounts:getAccountInfo`,
  `POST /v1/accounts:lookup`.
- **v3 legacy:** `POST /identitytoolkit/v3/relyingparty/verifyPassword`,
  `POST .../signupNewUser`, `POST .../getAccountInfo`, `POST .../refreshToken`.
- Returns `{localId, idToken, refreshToken, expiresIn, email}`.
- Users are **stateful** — a user created via signUp persists and can sign in.
- **idTokens are bound to their user:** every issued `idToken` maps to the uid
  it was minted for (1h expiry). `getAccountInfo` / `lookup` resolve the
  presented token to ITS user — never "the first user" — and an unknown or
  expired token returns **401** `INVALID_ID_TOKEN`.
- **Refresh tokens are stateful too:** every issued `refreshToken` is bound to
  its user, and `POST .../relyingparty/refreshToken` (body `refresh_token` or
  `refreshToken`) exchanges it for fresh tokens:
  `{user_id, id_token, refresh_token, expires_in, token_type:"Bearer"}`.
  Unknown refresh tokens return `400 INVALID_REFRESH_TOKEN`.
- **Secure Token Service:** `POST /v1/token` (form-encoded
  `grant_type=refresh_token&refresh_token=...`) exchanges a VERIFIED inbound
  refresh token for freshly minted tokens (1h expiry), returning the real
  securetoken shape `{access_token, id_token, refresh_token, expires_in:"3600",
  token_type:"Bearer", user_id, project_id}`. Wrong grant types and unknown
  refresh tokens return `400`.

### Firestore

- `GET /v1/projects/{project}/databases/(default)/documents/{collection}` → list.
- `POST .../documents/{collection}` → create. Honors an explicit
  `?documentId=` (or body `documentId`); reusing an existing ID returns
  **409 `ALREADY_EXISTS`** like the real API.
- `GET .../documents/{collection}/{id}` → get.
- `PATCH .../documents/{collection}/{id}` → upsert (existing fields are merged).
- `DELETE .../documents/{collection}/{id}` → delete (200 with empty body;
  404 if the document does not exist).
- **Nested collection paths:** subcollections under a document are supported
  one level deep — `GET/POST .../documents/{collection}/{document}/{sub}` and
  `GET/PATCH/DELETE .../documents/{collection}/{document}/{sub}/{id}`. The
  document `name` carries the full resource path
  (`.../documents/users/alice/addresses/doc-1`).
- **`POST .../documents:runQuery`** — structuredQuery subset evaluated with
  query_select: `from:[{collectionId}]`, `where:{fieldFilter:{field:{fieldPath},
  op, value}}` (ops `EQUAL`, `NOT_EQUAL`, `GREATER_THAN`, `GREATER_THAN_OR_EQUAL`,
  `LESS_THAN`, `LESS_THAN_OR_EQUAL`, `IN`, `ARRAY_CONTAINS`),
  `orderBy:[{field, direction}]`, `limit`. The response is the real
  RunQueryResponse array `[{document, readTime}]`. Malformed queries (missing
  `structuredQuery`/`from`, unsupported ops) return `400 INVALID_ARGUMENT`.
- **Typed values:** every field is `{stringValue:"x"}`, `{integerValue:"5"}`,
  `{booleanValue:true}`, `{arrayValue:{values:[...]}}`, `{mapValue:{fields:{...}}}`.
- **Cursor pagination on list:** `GET .../documents/{collection}` accepts
  `pageSize` and `pageToken` query params; when more documents remain the
  response includes `nextPageToken`. Without `pageSize` the whole list is
  returned in one page.

### FCM (Cloud Messaging)

- `POST /v1/projects/{project}/messages:send` → `{name:"projects/.../messages/N"}`.
- **Targets:** exactly one of `message.token`, `message.topic`, or
  `message.condition` (zero or multiple targets → `400`). Conditions use the
  real syntax — `'news' in topics || 'sports' in topics` (union) and
  `'a' in topics && 'b' in topics` (intersection) — and are routed to the
  subscribed-token collections.
- **Topic subscriptions (simulator extension standing in for the Instance ID
  API):** `POST /v1/projects/{project}/topics/{topic}:subscribe` /
  `:unsubscribe` with body `{token}` or `{tokens:[...]}`.
- Sent messages are stored (stateful) with their resolved recipient tokens;
  `GET /v1/projects/{project}/messages` lists them (simulator extension, for
  asserting fanout).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v1/accounts:signInWithPassword` | `auth.star#on_sign_in_with_password` | Sign in (v1) |
| POST | `/v1/accounts:signUp` | `auth.star#on_sign_up` | Create user (v1) |
| POST | `/v1/accounts:signInWithIdp` | `auth.star#on_sign_in_with_idp` | Sign in with IDP |
| POST | `/v1/accounts:getAccountInfo` | `auth.star#on_get_account_info` | Get user info |
| POST | `/v1/accounts:lookup` | `auth.star#on_get_account_info` | Lookup user |
| POST | `/identitytoolkit/v3/relyingparty/{action}` | `auth.star#on_relyingparty` | v3 dispatcher (verifyPassword, signupNewUser, getAccountInfo, refreshToken) |
| POST | `/v1/token` | `auth.star#on_securetoken` | securetoken refresh_token exchange |
| POST | `.../documents:runQuery` | `firestore.star#on_run_query` | structuredQuery (from/where/orderBy/limit) |
| GET | `.../documents/{collection}` | `firestore.star#on_list_documents` | List docs |
| POST | `.../documents/{collection}` | `firestore.star#on_create_document` | Create doc (honors documentId) |
| GET | `.../documents/{collection}/{id}` | `firestore.star#on_get_document` | Get doc |
| PATCH | `.../documents/{collection}/{id}` | `firestore.star#on_upsert_document` | Upsert doc (merge) |
| DELETE | `.../documents/{collection}/{id}` | `firestore.star#on_delete_document` | Delete doc |
| GET/POST | `.../documents/{collection}/{document}/{sub}` | `firestore.star#on_list/on_create_subdocuments` | Subcollection list/create |
| GET/PATCH/DELETE | `.../documents/{collection}/{document}/{sub}/{id}` | `firestore.star#on_get/on_upsert/on_delete_subdocument` | Subcollection doc ops |
| POST | `/v1/projects/{project}/messages:send` | `fcm.star#on_send_message` | Send FCM (token/topic/condition) |
| GET | `/v1/projects/{project}/messages` | `fcm.star#on_list_messages` | List sent messages (extension) |
| POST | `/v1/projects/{project}/topics/{topic}:subscribe` | `fcm.star#on_subscribe` | Subscribe tokens to topic (extension) |
| POST | `/v1/projects/{project}/topics/{topic}:unsubscribe` | `fcm.star#on_unsubscribe` | Unsubscribe tokens (extension) |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `users` | Auth users (with email, password, localId) |
| `documents` | Firestore documents (with typed fields) |
| `messages` | Sent FCM messages |
| `subscriptions` | FCM topic subscriptions (token ↔ topic) |

## Auth

Endpoints accept either `Authorization: Bearer <token>` (OAuth2 access token) or a
`key` query/body parameter. Presence is checked; the value is not validated. A missing
auth credential returns `401` with `{error:{code, message, status}}`.

## Usage

```yaml
services:
  firebase:
    adapter: ./adapters/firebase-style
```

Then `stunt up` and make requests to the served address.
