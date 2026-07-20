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
| POST | `/common/oauth2/v2.0/token` | Token exchange: `authorization_code` and `refresh_token` grants. Returns `{token_type, expires_in, ext_expires_in, access_token, refresh_token, scope}`. |

### Graph (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v1.0/me` | Current user profile (`id`, `displayName`, `userPrincipalName`, `mail`, …). |
| GET | `/v1.0/users` | List directory users. |
| POST | `/v1.0/users` | Create a user (stateful). |
| GET | `/v1.0/users/{id}` | Get user by id or UPN. |
| GET | `/v1.0/applications` | List app registrations. |
| GET | `/v1.0/servicePrincipals` | List service principals (enterprise apps). |

## Key shapes

- Access tokens are JWT-shaped (`header.payload.signature`, synthetic encoding).
- User objects use `userPrincipalName` (UPN), not `email`.
- Listings use `"value": [...]` arrays with `@odata.context`.
- Admin consent is modeled via `prompt=admin_consent` on the authorize endpoint.

## Usage

```bash
stunt init
# Add to your stunt.yaml:
#   entra:
#     adapter: ./adapters/entra-id-style
stunt up
```

All data is synthetic. See [DISCLAIMER](DISCLAIMER).
