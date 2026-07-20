# signin-with-apple-style

A stunt adapter for simulating **Sign in with Apple** (OAuth2 + JWT) locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Apple. "Apple", "Sign in with Apple", and related marks are
> trademarks of their respective owners. See [DISCLAIMER](DISCLAIMER) for full
> terms. This adapter is for **local development and testing only**.

## What it simulates

A faithful structural mock of Apple's Sign in with Apple OAuth2 flow — the surface
that causes the most integration pain: JWT-signed client secrets, id_token
verification, and the JWKS endpoint.

- **Authorization:** `GET /auth/authorize` → 302 redirect with single-use auth code.
- **Token exchange:** `POST /auth/token` (authorization_code grant) →
  `{access_token, id_token, refresh_token}`.
- **Refresh:** `POST /auth/token` (refresh_token grant) → new access_token.
- **JWKS:** `GET /auth/keys` → public key set for id_token verification.
- **id_token:** A JWT with `{iss:"https://appleid.apple.com",aud,sub,email,...}`.
- **Stateful auth codes:** single-use (consumed on exchange).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/auth/authorize` | `oauth.star#on_authorize` | 302 redirect with code+state |
| POST | `/auth/token` | `oauth.star#on_token` | Token exchange + refresh |
| GET | `/auth/keys` | `oauth.star#on_get_keys` | JWKS public key set |

## JWT validation

This adapter performs **structural validation** of the `client_secret` JWT:

1. The `client_secret` form parameter must be a JWT (3 dot-separated segments).
2. The JOSE header (segment 0) is **base64url-decoded** and checked to contain
   `ES256` (the `alg` claim).

**Signature crypto is NOT verified.** Real ECDSA signature verification is the
documented stretch goal.

The **id_token** returned by the token endpoint is a structurally valid JWT with
an ES256 JOSE header. Its payload contains the standard Sign in with Apple claims:
`iss`, `aud`, `sub`, `email`, `email_verified`, `is_private_email`.

Real Sign in with Apple client_secrets are signed ES256 with header
`{alg:"ES256",kid:<keyId>,typ:"JWT"}` and payload
`{iss:<teamId>,iat,exp,aud:"https://appleid.apple.com",sub:<clientId>}`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  apple:
    adapter: ./adapters/signin-with-apple-style
```

Then `stunt up` and run through the OAuth2 flow.
