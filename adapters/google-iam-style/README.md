# google-iam-style

A stunt adapter simulating the **Google Cloud IAM API** with service accounts, JWT-bearer token exchange, and domain-wide delegation, for local testing.

## Simulated API

- **Name:** Google Cloud IAM API + Service Accounts
- **Version:** `v1`

## Endpoints

### OAuth2 JWT-bearer exchange

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/oauth2/v4/token` | Service-account JWT exchange (`grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer`). Returns `{access_token, expires_in, token_type}` plus an `id_token` when the scope includes `openid`. |
| GET | `/oauth2/v3/certs` | JWKS for the signing key (Google's real discovery path). |

### Service Accounts (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v1/projects/{project}/serviceAccounts` | List service accounts. |
| POST | `/v1/projects/{project}/serviceAccounts` | Create a service account (stateful). |
| GET | `/v1/projects/{project}/serviceAccounts/{sa}` | Get a service account. |
| DELETE | `/v1/projects/{project}/serviceAccounts/{sa}` | Delete a service account. |
| GET | `/v1/projects/{project}/serviceAccounts/{sa}/keys` | List service-account keys. |
| POST | `/v1/projects/{project}/serviceAccounts/{sa}:generateAccessToken` | Mint a short-lived access token. |

### IAM Roles (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v1/projects/{project}/roles:queryGrantableRoles` | Query grantable roles for a resource. |

## Key shapes

- Service accounts: `accounts:[{name, projectId, uniqueId, email, displayName, oauth2ClientId, disabled}]`.
- Service-account emails follow `<id>@<project>.iam.gserviceaccount.com`.
- JWT-bearer assertions are **verified cryptographically** (not just parsed):
  RS256 signature over `header.payload` against the fixed synthetic Google
  public key (served at `GET /oauth2/v3/certs`), plus claim checks — `iss`
  non-empty, `exp` in the future, `aud == https://oauth2.googleapis.com/token`.
  A malformed, forged, or expired assertion returns
  `400 {"error":"invalid_grant"}`.
- When the granted scope includes `openid`, the exchange returns a **real
  RS256 id_token** (`iss:"https://accounts.google.com"`, `azp`/`aud`/`sub` =
  the service-account email, `iat`/`exp` ~1h) signed with the same keypair.
- Minted `ya29.*` access tokens are validated against the `tokens` store and
  carry an `expires_at` (1h, matching `expires_in`); stale tokens 401.
- The fixed synthetic RSA-2048 keypair (kid `mock-google-key-1`, shared with
  the google-style adapter) lives in `scripts/lib.star`; it is throwaway mock
  material that exists nowhere but this repository.
- Models the service-account + domain-wide-delegation confusion as deterministic state.

## Usage

```bash
stunt init
# Add to your stunt.yaml:
#   google-iam:
#     adapter: ./adapters/google-iam-style
stunt up
```

All data is synthetic. See [DISCLAIMER](DISCLAIMER).
