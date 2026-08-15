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
- **id_token:** A REAL ES256-signed JWT with
  `{iss:"https://appleid.apple.com",aud,sub,email,iat,exp,...}`.
- **Client-secret verification:** the `client_secret` JWT is verified
  cryptographically (ES256 signature + `aud` + `exp` claims).
- **Stateful auth codes:** single-use (consumed on exchange).

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| GET | `/auth/authorize` | `oauth.star#on_authorize` | 302 redirect with code+state |
| POST | `/auth/token` | `oauth.star#on_token` | Token exchange + refresh |
| GET | `/auth/keys` | `oauth.star#on_get_keys` | JWKS public key set (real P-256 key) |

## JWT verification (real crypto)

This adapter performs **full cryptographic verification** of the inbound
`client_secret` JWT, exactly the way Apple's token endpoint does (modulo the
fixed mock key):

1. The `client_secret` form parameter must be a JWT (3 dot-separated segments).
2. The JOSE header (decoded via `crypto.base64url_decode` + `json.decode`) must
   carry `alg:"ES256"`.
3. The ES256 signature over `header.payload` is verified with
   `crypto.ecdsa_verify_p256` against the adapter's fixed synthetic P-256
   public key.
4. Claims are enforced: `aud` must be `https://appleid.apple.com` and `exp`
   must be in the future (checked against `clock.now_unix()`).

A malformed, forged, or expired `client_secret` returns
`400 {"error":"invalid_client"}`.

The **id_token** returned by the token endpoint is a real ES256 JWT signed with
the private half of the same keypair (raw `r||s` signature). Its payload
contains the standard Sign in with Apple claims: `iss`, `aud`, `sub`, `email`,
`email_verified`, `is_private_email`, `auth_time`, `iat`, `exp` (1 hour),
`nonce_supported`. The public half is served at `GET /auth/keys` (via
`crypto.ec_public_jwk`), so any standards-compliant JWT library can verify it.

### Test key material

The fixed synthetic EC P-256 keypair lives in `scripts/lib.star`
(`_JWT_PRIVATE_KEY` / `_JWT_PUBLIC_KEY`, kid `mock-siwa-key-1`). It is
throwaway mock material that exists nowhere else. To mint a client_secret
accepted by this adapter, sign an ES256 JWT with the private key
(PKCS#8 PEM above) and payload
`{iss:<teamId>,iat,exp,aud:"https://appleid.apple.com",sub:<clientId>}` —
see `internal/engine/signin_with_apple_style_test.go` for a worked example.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  apple:
    adapter: ./adapters/signin-with-apple-style
```

Then `stunt up` and run through the OAuth2 flow.
