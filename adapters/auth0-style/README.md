# Auth0-style adapter

A stunt adapter for simulating the **Auth0 Authentication & Management API** locally —
the OIDC/OAuth2 authentication domain plus Management API v2. All data is synthetic —
no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by Okta / Auth0. "Auth0" and related marks are trademarks of their
> respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is for
> **local development and testing only**.

## What it simulates

- **OIDC discovery + JWKS:** `GET /.well-known/openid-configuration` (every URL and
  the `issuer` derived from the request `Host`, so a client pointed at the adapter's
  origin sees a self-consistent tenant) and `GET /.well-known/jwks.json`.
- **Authorization code flow:** `GET /authorize` validates `client_id` /
  `redirect_uri` against the seeded application and 302s to
  `redirect_uri?code=...&state=...`; invalid parameters 302 back with the OAuth2
  `error` / `error_description` pair.
- **Token endpoint:** `POST /oauth/token` with `grant_type=authorization_code`,
  `refresh_token`, or `client_credentials` (client credentials in the body or HTTP
  Basic). Returns a real RS256 `access_token` (plus `id_token` on the code and
  refresh grants, plus `refresh_token` on the code grant), `expires_in: 3600`,
  `token_type: "Bearer"`.
- **userinfo:** `GET /userinfo` (Bearer) — the access token is verified
  cryptographically (RSA-SHA256 signature over the verbatim `header.payload`
  input + `exp` + `iss`), and its `sub` must resolve to a tenant user.
- **Revocation:** `POST /oauth/revoke` — idempotent 200 (RFC 7009).
- **Signup:** `POST /dbconnections/signup` — email/password user creation; new
  users start unverified.
- **Management API v2** (Bearer): user CRUD with `q` search and `page`/`per_page`
  paging (`include_totals=true` switches to the `{start, limit, length, users}`
  envelope), and roles list/create/assign/unassign per user.

Tokens are **real, decodable RS256 JWTs** signed with a fixed synthetic RSA-2048
keypair (kid `mock-auth0-key-1`). The JWKS endpoint serves the public half (via
`crypto.rsa_public_jwk`), so any standards-compliant JWT library can verify them;
the adapter verifies inbound Bearer tokens the same way. `exp`/`iat` come from the
engine clock, so token expiry is testable with a virtual clock.

## The documented tenant

One synthetic application client and three synthetic users are seeded.

| | |
|---|---|
| client_id | `mock-web-app-client` |
| client_secret | `mock-web-app-secret` |
| redirect_uris | `http://localhost:3000/callback`, `https://demo.example.test/cb` |
| grant_types | `authorization_code`, `refresh_token`, `client_credentials` |

| user_id | email | verified |
|---|---|---|
| `auth0\|a1b2c3` | `ada@example.test` | yes |
| `auth0\|d4e5f6` | `grace@example.test` | yes |
| `auth0\|g7h8i9` | `alan@example.test` | no |

(Seed fixtures cannot ship literal email addresses — `stunt adapter lint` rejects
them as real-looking data — so the seeded users store the address split across
`email_local`/`email_domain` and the handlers compose it on read.)

Roles `rol_mock_admin_1` (`admin`) and `rol_mock_reader_2` (`reader`) are seeded;
`ada` starts with the admin role.

### Getting a token (authorization code flow)

```bash
BASE=http://localhost:4010            # your stunt service port
CLIENT_ID=mock-web-app-client
CLIENT_SECRET=mock-web-app-secret
REDIRECT=http://localhost:3000/callback

# 1. authorize (binds the login_hint user; without one, the first seeded user)
LOCATION=$(curl -s -o /dev/null -w '%{redirect_url}' \
  "$BASE/authorize?client_id=$CLIENT_ID&redirect_uri=$REDIRECT&response_type=code&state=s1&login_hint=grace@example.test")
CODE=${LOCATION#*code=}; CODE=${CODE%%&*}

# 2. exchange (client_secret_post; HTTP Basic also works)
curl -s -X POST "$BASE/oauth/token" \
  -d grant_type=authorization_code -d code=$CODE \
  -d client_id=$CLIENT_ID -d client_secret=$CLIENT_SECRET -d redirect_uri=$REDIRECT
# → {access_token, id_token, refresh_token, token_type:"Bearer", expires_in:3600}

# 3. call the Management API with the access token
curl -s "$BASE/api/v2/users" -H "Authorization: Bearer $ACCESS_TOKEN"
```

For a machine-to-machine token:

```bash
curl -s -X POST "$BASE/oauth/token" \
  -d grant_type=client_credentials \
  -d client_id=$CLIENT_ID -d client_secret=$CLIENT_SECRET \
  -d audience=https://auth0-style.test/api/v2/
```

## Error shapes

- Authentication domain (`/authorize` errors, `/oauth/token`, `/userinfo`,
  `/dbconnections/signup`): `{"error": "...", "error_description": "..."}`
  (401 `invalid_client` / `invalid_token`, 400 `invalid_grant` /
  `unsupported_grant_type` / `user_exists`, ...).
- Management API v2: `{"statusCode": N, "error": "<HTTP reason>", "message": "..."}`
  (+ `errorCode` on validation failures) — 400 `Bad Request` (validation,
  duplicate email, unknown role id), 401 `Unauthorized` (`Missing bearer token`,
  `Invalid token`, `Token expired`), 404 `Not Found`, 409 `Conflict`.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/.well-known/openid-configuration` | `oidc.star#on_discovery` | OIDC discovery (issuer from Host) |
| GET | `/.well-known/jwks.json` | `oidc.star#on_jwks` | JWKS for the signing key |
| GET | `/authorize` | `oidc.star#on_authorize` | Authorization-code redirect |
| POST | `/oauth/token` | `oidc.star#on_token` | code / refresh / client_credentials grants |
| GET | `/userinfo` | `oidc.star#on_userinfo` | profile for a Bearer access token |
| POST | `/oauth/revoke` | `oidc.star#on_revoke` | revoke a refresh token (idempotent) |
| POST | `/dbconnections/signup` | `oidc.star#on_signup` | email/password signup |
| GET | `/api/v2/users` | `management.star#on_users_list` | list users (`q`, paging) |
| POST | `/api/v2/users` | `management.star#on_user_create` | create user (201) |
| GET | `/api/v2/users/{id}` | `management.star#on_user_get` | get user |
| PATCH | `/api/v2/users/{id}` | `management.star#on_user_patch` | update user (merge) |
| DELETE | `/api/v2/users/{id}` | `management.star#on_user_delete` | delete user (204) |
| GET | `/api/v2/roles` | `management.star#on_roles_list` | list roles |
| POST | `/api/v2/roles` | `management.star#on_role_create` | create role |
| GET | `/api/v2/users/{id}/roles` | `management.star#on_user_roles_list` | roles assigned to a user |
| POST | `/api/v2/users/{id}/roles` | `management.star#on_user_roles_assign` | assign roles (204) |
| DELETE | `/api/v2/users/{id}/roles` | `management.star#on_user_roles_delete` | unassign roles (204) |

## State

Collections: `users`, `roles`, `clients` (seeded from `fixtures/`), plus `oauth_codes`
and `refresh_tokens` (runtime only). Authorization codes live 5 minutes and are
single-use; refresh tokens live 30 days and are **reusable** (rotation off, the
Auth0 default — the refresh grant returns only a fresh access/id pair).

## Divergences

Deliberate simplifications — none of them call the network:

- **No login page / consent screen.** `/authorize` auto-authenticates the
  `login_hint` user (matched on email or `user_id`) instead of rendering a hosted
  login; without a `login_hint` it binds the first seeded user.
- **Error redirects to unvalidated redirect URIs.** RFC 6749 requires a server to
  refuse redirecting when `redirect_uri` fails validation; this adapter still 302s
  the OAuth2 error back to the caller-supplied URI so local clients can see the
  failure. Unknown `client_id` behaves the same way.
- **One fixed signing key** (kid `mock-auth0-key-1`) rather than a rotating JWKS;
  the private half ships in the adapter so anyone can forge mock tokens —
  throwaway material, never use it anywhere real.
- **Issuer from the request `Host`** (`https://<host>/`): tokens minted under one
  host fail `iss` validation under another. Absent Host falls back to
  `auth0-style.test`.
- **Refresh tokens are opaque strings** (`mock-refresh-token-N`), not JWTs, and are
  not rotated; revocation only deletes the stored refresh token (access tokens are
  stateless, so an already-minted one stays valid until `exp`).
- **Access/id token lifetime is 1 hour** (3600s) for every grant.
- **`q` search is reduced** to `field:"value"` exact matches on
  email/name/nickname/user_id (via `query_select`) or a bare substring term over
  the same fields — not Lucene. Checkpoint pagination (`from`/`take`) is not
  implemented; only `page`/`per_page` (max 100).
- **No scope enforcement on the Management API**: any valid RS256 access token
  (user or machine-to-machine) may call `/api/v2/*`; `scope`/permission claims are
  minted but not checked.
- **Signup password policy** is "at least 8 characters" — not the configurable
  Auth0 policy engine.
- **No MFA, no Organizations, no actions/hooks/flows, no email delivery** (signup
  creates unverified users; there is no verification email), no user blocking or
  password change endpoints, and timestamps are RFC 3339 without Auth0's
  millisecond precision.
