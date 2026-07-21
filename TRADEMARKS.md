# Trademark Policy

Stunt is an independent, open-source project for creating **local simulators** of
public APIs so developers can test their client code without creating remote
accounts or hitting the network.

## Our use of third-party names

Stunt ships "reference adapters" that reproduce the *structure* of real-world APIs
so your client code can run against a realistic local mock. Adapter names take the
form **`<provider>-style`** (for example, `stripe-style`, `google-style`,
`twilio-style`) and their documentation may reference the provider's name and the
name of its API.

We use these names **nominatively** — that is, to refer to the actual provider and
API whose behavior is being simulated. This is the same kind of descriptive use that
allows a product to say it is "compatible with" or "for" another product. Without
the provider's name, it would be impossible to describe which API an adapter
simulates.

## We are not affiliated

- Stunt is **not affiliated with, endorsed by, or sponsored by** any of the
  providers whose names appear in adapter names.
- Adapters are **unofficial** and contain **only synthetic, fabricated data**.
  They never call the real API, contain no real provider data, no recorded
  responses, and no proprietary documentation.
- Every branded adapter ships with a `DISCLAIMER` file stating the above.
- Stunt **does not use provider logos, wordmarks, or other branded trade dress**.
  Only plain-text names, and only as much of the name as is necessary to identify
  the API.

## The `-style` suffix

The `-style` suffix is deliberate. It signals that an adapter is an *imitation in
the style of* the real API — not the real thing and not an official product.

## Renaming on request

We respect providers' brands. If you are a trademark holder and would like an
adapter renamed or removed, please open an issue or email the maintainers. Adapter
names are purely labels; renaming is trivial and does not affect functionality. We
will respond promptly and in good faith.

## Summary

This policy reflects established norms in the developer-tooling community (cf.
local cloud-API emulators and mock libraries) and is intended to make clear that
Stunt's use of third-party names is descriptive, non-commercial, and does not
suggest any endorsement or affiliation.
