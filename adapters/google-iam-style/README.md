# google-iam-style

A stunt adapter simulating the **Google Cloud IAM API** with service accounts, JWT-bearer token exchange, and domain-wide delegation, for local testing.

## Simulated API

- **Name:** Google Cloud IAM API + Service Accounts
- **Version:** `v1`

## Endpoints

### OAuth2 JWT-bearer exchange

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/oauth2/v4/token` | Service-account JWT exchange (`grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer`). Returns `{access_token, expires_in, token_type}`. |

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
- JWT-bearer exchange accepts a synthetic signed JWT assertion and mints `ya29.*` access tokens.
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
