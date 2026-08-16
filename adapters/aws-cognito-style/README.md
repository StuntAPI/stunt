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
  The code is bound to an **existing user**: the `login_hint` (or `username`) query
  parameter when present, else the seeded `demo-user`. Unknown `login_hint` users and
  non-`code` response types get OAuth error redirects (`error=invalid_request`,
  `error=unsupported_response_type`), never minted users or codes.
- `POST /oauth2/token` → `{access_token, id_token, token_type, expires_in}`.
  - `grant_type=authorization_code` also returns a `refresh_token`.
  - `grant_type=refresh_token` returns a **fresh RS256 access/id pair only** — real
    Cognito (rotation off, the default) does not issue or return a new refresh token;
    the presented one stays valid for 30 days and can be reused.
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

- `SignUp` → `{UserConfirmed, UserSub, CodeDeliveryDetails}` (user lands `UNCONFIRMED`).
- `ConfirmSignUp` → confirms the user with the **deterministic code** (below);
  wrong code → `CodeMismatchException`, already confirmed → `NotAuthorizedException`.
- `InitiateAuth` → password login, refresh grants, and challenges:
  - `USER_PASSWORD_AUTH` → `{AuthenticationResult:{AccessToken, IdToken, RefreshToken, ExpiresIn}, ChallengeParameters}`.
  - `REFRESH_TOKEN_AUTH` / `REFRESH_TOKEN_REFRESH` (`AuthParameters.REFRESH_TOKEN`) →
    fresh access/id tokens only, **no new refresh token** (reusable, real Cognito
    default). Invalid/revoked tokens → `NotAuthorizedException`.
  - `FORCE_CHANGE_PASSWORD` users signing in with their temporary password get
    `{ChallengeName:"NEW_PASSWORD_REQUIRED", Session, ChallengeParameters:{USER_ID_FOR_SRP}}`
    instead of tokens.
- `RespondToAuthChallenge` / `AdminRespondToAuthChallenge`
  (`NEW_PASSWORD_REQUIRED` only) → validates the `Session` (single-use, expires after
  AuthSessionValidity = 3 minutes), enforces the password policy on `NEW_PASSWORD`
  (`InvalidPasswordException` otherwise), sets + confirms the password, and returns
  tokens. Unknown/expired/replayed sessions → `NotAuthorizedException` ("Invalid
  session for the user, session is expired").
- `AdminInitiateAuth` → admin flows (`ADMIN_USER_PASSWORD_AUTH`, `ADMIN_NO_SRP_AUTH`,
  plus the refresh flows). The plain `InitiateAuth` rejects the `ADMIN_*` flows and
  vice versa (`InvalidParameterException`).
- `ForgotPassword` → `{CodeDeliveryDetails:{AttributeName, DeliveryMedium, Destination}}`,
  opens a 1-hour reset-code window.
- `ConfirmForgotPassword` → real Cognito error ladder: 5 wrong codes →
  `LimitExceededException` ("Attempt limit exceeded, please try after some time."),
  lapsed window → `ExpiredCodeException`, wrong code → `CodeMismatchException`,
  weak password → `InvalidPasswordException`. Success sets + confirms the new password.
- `GlobalSignOut` (`AccessToken`) and `AdminUserGlobalSignOut` (`UserPoolId` +
  `Username`) → revoke **every** access and refresh token for the user (the token
  bindings are deleted from the store). Afterwards `GetUser` /
  `/oauth2/userInfo` fail with `NotAuthorizedException` / `401 invalid_token`, and
  refresh grants fail with `invalid_grant`. Unknown username →
  `ResourceNotFoundException`.
- `GetUser` (AccessToken) → `{Username, UserAttributes:[{Name,Value}]}`.
- `ListUsers` → `{Users:[{Username, Attributes, UserStatus}]}`.
- `AdminCreateUser` → creates a `FORCE_CHANGE_PASSWORD` user with a temporary
  password (`TemporaryPassword` param, or a deterministic default) — the first
  password login triggers `NEW_PASSWORD_REQUIRED`.

### Identity pool (federated identities)

- `GetId` → `{IdentityId}`.
- `GetCredentialsForIdentity` → `{Credentials:{AccessKeyId, SecretKey, SessionToken, Expiration}}`.

### Seeded users

Two users are seeded once per instance (KV-guarded, the whatsapp-style
pattern) so the hosted-UI and challenge flows have deterministic subjects:

| Username             | Password       | Status                 |
|----------------------|----------------|------------------------|
| `demo-user`          | `DemoPass123!` | `CONFIRMED`            |
| `force-change-user`  | `TempPass1A!`  | `FORCE_CHANGE_PASSWORD` |

### Deterministic verification codes

`SignUp`/`ConfirmSignUp` and `ForgotPassword`/`ConfirmForgotPassword` use the
twilio-verify convention: the code is the **last 6 digits found in the
username, zero-padded** (`forgot-user-42` → `000042`; digit-free usernames →
`000000`). Reset codes are valid for 1 hour; five failed confirmations lock
the attempt (`LimitExceededException`).

### Error shapes

Cognito uses the distinctive `{"__type":"NotAuthorizedException","message":"..."}` error
envelope (reproduced exactly).

Users, tokens, authorization codes, and challenge sessions are **stateful**.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/oauth2/authorize` | `oauth.star#on_authorize` | Auth code redirect (302, binds existing users) |
| POST | `/oauth2/token` | `oauth.star#on_token` | Token exchange (authorization_code / refresh_token) |
| GET | `/oauth2/userInfo` | `oauth.star#on_user_info` | User info (Bearer) |
| GET | `/login` | `oauth.star#on_login` | Hosted UI login |
| GET | `/logout` | `oauth.star#on_logout` | Hosted UI logout |
| POST | `/` | `service.star#on_service_api` | Service API (X-Amz-Target dispatch) |
| GET | `/{userPoolId}/.well-known/jwks.json` | `oauth.star#on_jwks` | Pool JWKS (real RSA key) |

## Backing stores

| Collection | Purpose |
|------------|---------|
| `users` | User pool users (username, sub, attributes, password, status) |
| `tokens` | Access/refresh token → user binding (for GetUser / userInfo; deleted on GlobalSignOut; refresh tokens carry a 30-day expiry) |
| `oauth_codes` | Hosted-UI authorization codes (single-use) |
| `auth_sessions` | Challenge sessions (`NEW_PASSWORD_REQUIRED`), 3-minute expiry, single-use |

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
