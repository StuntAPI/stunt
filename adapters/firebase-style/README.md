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

### Firestore

- `GET /v1/projects/{project}/databases/(default)/documents/{collection}` → list.
- `POST .../documents/{collection}` → create.
- `GET .../documents/{collection}/{id}` → get.
- `PATCH .../documents/{collection}/{id}` → upsert.
- **Typed values:** every field is `{stringValue:"x"}`, `{integerValue:"5"}`,
  `{booleanValue:true}`, `{arrayValue:{values:[...]}}`, `{mapValue:{fields:{...}}}`.

### FCM (Cloud Messaging)

- `POST /v1/projects/{project}/messages:send` → `{name:"projects/.../messages/N"}`.
- Sent messages are stored (stateful).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/v1/accounts:signInWithPassword` | `auth.star#on_sign_in_with_password` | Sign in (v1) |
| POST | `/v1/accounts:signUp` | `auth.star#on_sign_up` | Create user (v1) |
| POST | `/v1/accounts:signInWithIdp` | `auth.star#on_sign_in_with_idp` | Sign in with IDP |
| POST | `/v1/accounts:getAccountInfo` | `auth.star#on_get_account_info` | Get user info |
| POST | `/v1/accounts:lookup` | `auth.star#on_get_account_info` | Lookup user |
| POST | `/identitytoolkit/v3/relyingparty/{action}` | `auth.star#on_relyingparty` | v3 dispatcher (verifyPassword, signupNewUser, getAccountInfo, refreshToken) |
| GET | `.../documents/{collection}` | `firestore.star#on_list_documents` | List docs |
| POST | `.../documents/{collection}` | `firestore.star#on_create_document` | Create doc |
| GET | `.../documents/{collection}/{id}` | `firestore.star#on_get_document` | Get doc |
| PATCH | `.../documents/{collection}/{id}` | `firestore.star#on_upsert_document` | Upsert doc |
| POST | `/v1/projects/{project}/messages:send` | `fcm.star#on_send_message` | Send FCM |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `users` | Auth users (with email, password, localId) |
| `documents` | Firestore documents (with typed fields) |
| `messages` | Sent FCM messages |

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
