# entra-id-style

A stunt adapter simulating the **Microsoft Entra ID (Azure AD)** API via Microsoft Graph, for local testing.

## Simulated API

- **Name:** Microsoft Graph / Entra ID
- **Version:** `v1.0`

## Endpoints

### OAuth2 (Microsoft identity platform v2.0)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/common/oauth2/v2.0/authorize` | Authorization code redirect (302) with `code` + `state` + `session_state`. Supports `prompt=admin_consent`. |
| POST | `/common/oauth2/v2.0/token` | Token exchange: `authorization_code` and `refresh_token` grants. Returns `{token_type, expires_in, ext_expires_in, access_token, refresh_token, scope}`. The `access_token` is a real RS256-signed JWT (verifiable against the JWKS below); `refresh_token` is only issued when the `offline_access` scope was requested. |
| GET | `/common/discovery/v2.0/keys` | JWKS: the RSA public key (as `{keys: [...]}` with `kid`, `alg: RS256`, `use: sig`) matching the fixed mock signing key, so a server-side token-verification flow runs end-to-end. |

### Graph (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v1.0/me` | Current user profile (`id`, `displayName`, `userPrincipalName`, `mail`, …). |
| GET | `/v1.0/users` | List directory users. Supports `$top`/`$skipToken` paging via `@odata.nextLink`. |
| POST | `/v1.0/users` | Create a user (stateful, returns 201). |
| GET | `/v1.0/users/{id}` | Get user by id or UPN. |
| GET | `/v1.0/applications` | List app registrations. Supports `$top`/`$skipToken` paging via `@odata.nextLink`. |
| GET | `/v1.0/servicePrincipals` | List service principals (enterprise apps). Supports `$top`/`$skipToken` paging via `@odata.nextLink`. |

## Key shapes

- Access tokens are real, verifiable JWTs (`header.payload.signature`): signed with RS256 using a fixed mock RSA keypair (`kid: mock-entra-kid-1`), with claims `sub`, `name`, `scp`, `nonce`, and `iss: https://login.microsoftonline.com/mock-tenant/v2.0`. Verify them against `GET /common/discovery/v2.0/keys`.
- User objects use `userPrincipalName` (UPN), not `email`.
- Listings use `"value": [...]` arrays with `@odata.context`.
- Admin consent is modeled via `prompt=admin_consent` on the authorize endpoint.

## Pagination

List endpoints (`/v1.0/users`, `/v1.0/applications`, `/v1.0/servicePrincipals`) follow Microsoft Graph's OData cursor convention: pass `$top` (page size) and round-trip the opaque `$skipToken` from `@odata.nextLink`. When `$top` is missing or `<= 0`, paging is disabled and the whole list is returned (no `@odata.nextLink`) — matching unpaginated Graph-style behavior.

## Usage

```bash
stunt init
# Add to your stunt.yaml:
#   entra:
#     adapter: ./adapters/entra-id-style
stunt up
```

All data is synthetic. See [DISCLAIMER](DISCLAIMER).
