# apple-apns-style

A stunt adapter for simulating the **Apple Push Notification service (APNs)**
HTTP/2 API locally. All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Apple. "Apple", "APNs", and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful structural mock of Apple's APNs HTTP/2 push notification API — the
surface that causes pain for push-notification integrations: provider token
authentication, device token management, and the cryptic error responses.

- **Provider token auth:** `authorization: bearer <jwt>` — verified
  cryptographically (real ES256 signature + expiry).
- **Send notification:** `POST /3/device/{deviceToken}` with JSON `{"aps":{...}}` body.
- **Success response:** `200` with `apns-id` header.
- **Error responses:** `400 {"reason":"BadDeviceToken"}`, `410 {"reason":"Unregistered"}`.
- **Device tracking:** unknown device tokens return `BadDeviceToken`.
- **Stateful notifications:** sent notifications are stored per device.

## Endpoints

| Method | Route | Handler | Description |
|--------|-------|---------|-------------|
| POST | `/3/device/{deviceToken}` | `send.star#on_send` | Send push notification |
| GET | `/3/device/{deviceToken}/notifications` | `send.star#on_get_notifications` | Retrieve sent notifications (internal) |

## JWT validation (real crypto)

This adapter performs **full cryptographic verification** of the provider JWT:

1. The `authorization: bearer <jwt>` header must be present.
2. The JWT must have 3 dot-separated, base64url-valid segments.
3. The JOSE header (decoded via `crypto.base64url_decode` + `json.decode`)
   must carry `alg:"ES256"`.
4. The ES256 signature over `header.payload` is verified with
   `crypto.ecdsa_verify_p256` against the adapter's fixed synthetic EC P-256
   public key (the locally "registered" provider key).
5. The token must not be expired: the `exp` claim when present, otherwise
   `iat` + 1 hour (real APNs caps provider-token lifetime at one hour).
   Expired tokens get `403 {"reason":"ExpiredProviderToken"}`.

Real APNs provider tokens are signed ES256 with header
`{alg:"ES256",kid:<keyId>}` and payload `{iss:<teamId>,iat:<timestamp>}`.

### Test key material

The fixed synthetic EC P-256 provider keypair is documented here for tests:
the public half is baked into `scripts/lib.star` (`_JWT_PUBLIC_KEY`) and the
private half (below) signs provider tokens the way a real provider key would.
It is throwaway mock material that exists nowhere else.

```
-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgz399eDP4CEo1JoR7
A5uHueHShJKhKvna8BiAvVQPkvyhRANCAAS6OMBYKYI6moCMo0FeQ23CAvQMT5sy
MZrf7jMKmvhmI/aMJuodNWq4eLSq6/X4oWriaY7RsKxIrQ5F/Ql+y6XJ
-----END PRIVATE KEY-----
```

See `internal/engine/apple_apns_style_test.go` for a worked example.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  apns:
    adapter: ./adapters/apple-apns-style
```

Then `stunt up` and send push notifications with a JWT bearer token.
