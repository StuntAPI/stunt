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

- **Provider token auth:** `authorization: bearer <jwt>` — validated structurally.
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

## JWT validation

This adapter performs **structural validation** of the provider JWT:

1. The `authorization: bearer <jwt>` header must be present.
2. The JWT must have 3 dot-separated segments.
3. The JOSE header (segment 0) is **base64url-decoded** and checked to contain
   `ES256` (the `alg` claim).

**Signature crypto is NOT verified.** Real ECDSA signature verification is the
documented stretch goal.

Real APNs provider tokens are signed ES256 with header
`{alg:"ES256",kid:<keyId>}` and payload `{iss:<teamId>,iat:<timestamp>}`.

## Usage

Point a `stunt.yaml` service at this directory:

```yaml
services:
  apns:
    adapter: ./adapters/apple-apns-style
```

Then `stunt up` and send push notifications with a JWT bearer token.
