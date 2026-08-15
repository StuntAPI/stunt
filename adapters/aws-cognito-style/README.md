# Amazon Cognito-style adapter

A stunt adapter for simulating the **Amazon Cognito Identity Provider API** locally —
both the hosted-UI OAuth flow and the service API (user pool + identity pool). All data
is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed by, or
> sponsored by Amazon Web Services. "Amazon Cognito" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This adapter is
> for **local development and testing only**.

## What it simulates

### Hosted UI OAuth flow

- `GET /oauth2/authorize` → 302 redirect to `redirect_uri?code=CODE&state=STATE`.
- `POST /oauth2/token` → `{access_token, id_token, refresh_token, token_type, expires_in}`.
  Supports `authorization_code` and `refresh_token` grants.
- `GET /oauth2/userInfo` (Bearer) → `{sub, username, email, ...}`.
- `GET /login` → 302 to `/oauth2/authorize`.
- `GET /logout` → 302 to `redirect_uri`.
- `GET /{userPoolId}/.well-known/jwks.json` → JWKS for the pool's signing key.

`id_token` and `access_token` are **real, decodable RS256 JWTs** signed with a
fixed synthetic RSA-2048 keypair (kid `mock-cognito-key-1`). Access tokens
carry `{sub, iss, client_id, token_use:"access", username, jti, iat, exp}`;
id tokens carry `{sub, iss, aud, token_use:"id", cognito:username, email,
email_verified, auth_time, iat, exp}`. The JWKS endpoint serves the public
half (via `crypto.rsa_public_jwk`), so any standards-compliant JWT library
can verify them, and the adapter itself verifies inbound access/id tokens
(signature + `exp` + `iss` + `token_use`) on `/oauth2/userInfo` and
`GetUser` — not just a store lookup.

### Service API (user pool — `X-Amz-Target`)

- `SignUp` → `{UserConfirmed, UserSub, CodeDeliveryDetails}`.
- `InitiateAuth` (`USER_PASSWORD_AUTH`) → `{AuthenticationResult:{AccessToken, IdToken, RefreshToken, ExpiresIn}, ChallengeParameters}`.
- `RespondToAuthChallenge` → returns auth result.
- `ConfirmSignUp` → confirms user.
- `GetUser` (AccessToken) → `{Username, UserAttributes:[{Name,Value}]}`.
- `ListUsers` → `{Users:[{Username, Attributes, UserStatus}]}`.
- `AdminCreateUser` → creates user as admin.

### Identity pool (federated identities)

- `GetId` → `{IdentityId}`.
- `GetCredentialsForIdentity` → `{Credentials:{AccessKeyId, SecretKey, SessionToken, Expiration}}`.

### Error shapes

Cognito uses the distinctive `{"__type":"NotAuthorizedException","message":"..."}` error
envelope (reproduced exactly).

Users and tokens are **stateful**.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth2/authorize` | `oauth.star#on_authorize` | Auth code redirect (302) |
| POST | `/oauth2/token` | `oauth.star#on_token` | Token exchange |
| GET | `/oauth2/userInfo` | `oauth.star#on_user_info` | User info (Bearer) |
| GET | `/login` | `oauth.star#on_login` | Hosted UI login |
| GET | `/logout` | `oauth.star#on_logout` | Hosted UI logout |
| POST | `/` | `service.star#on_service_api` | Service API (X-Amz-Target dispatch) |
| GET | `/{userPoolId}/.well-known/jwks.json` | `oauth.star#on_jwks` | Pool JWKS (real RSA key) |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `users` | User pool users (username, sub, attributes, password, status) |
| `tokens` | Access token → user binding (for GetUser / userInfo) |
| `oauth_codes` | Hosted-UI authorization codes (single-use) |

## Auth

- **Hosted UI endpoints** (`/oauth2/authorize`, `/oauth2/token`) are unauthenticated
  (they ARE the auth flow).
- **`/oauth2/userInfo`** requires `Authorization: Bearer <token>`.
- **Service API** uses `X-Amz-Target` header dispatch. SigV4 structural validation is
  applied when an `Authorization` header is present, but is not cryptographically enforced.
- **Inbound JWTs** (userInfo Bearer, `GetUser` AccessToken) are verified
  cryptographically: RS256 signature against the JWKS key, plus `exp`, `iss`
  and `token_use` claim checks. Tampered or expired tokens → `401 invalid_token`
  (hosted UI) / `NotAuthorizedException` (service API).

The fixed synthetic RSA keypair lives in `scripts/lib.star`
(`_JWT_PRIVATE_KEY` / `_JWT_PUBLIC_KEY`); it is throwaway mock material that
exists nowhere but this repository.

## Usage

```yaml
services:
  cognito:
    adapter: ./adapters/aws-cognito-style
```

Then `stunt up` and make requests to the served address.
