# First-party adapters

`stunt` ships a set of **reference adapters** so the tool is useful out of the box and so the
adapter model has proven examples. Each lives in its own directory under `adapters/`.

## Neutral naming & disclaimers (mandatory for branded adapters)

Adapters that mimic a specific company's API **MUST** follow these conventions, which keep the
project on the safe side of trademark/ToS concerns and make the "local testing simulator"
framing unambiguous:

1. **Neutral, "-style" naming.** The adapter id, directory name, and human description use the
   `<Provider>-style` form — e.g. `stripe-style`, `drive-style`, `twitter-style`, `dropbox-style`.
   Never claim to *be* the provider.
2. **`DISCLAIMER` file.** Every branded adapter directory ships a `DISCLAIMER` (see
   `DISCLAIMER.template`) stating: not affiliated with or endorsed by the provider; for **local
   development and testing only**; uses **synthetic data only** (no real provider data, no recorded
   responses); does not call the real provider.
3. **Synthetic data only.** All fixtures/templates are synthetic (faker-generated). `stunt adapter
   lint` must pass clean. This is the core safety property.
4. **Structure, not documentation.** Adapters reproduce the API *structure* (routes, shapes) needed
   for local testing; they do not reproduce the provider's proprietary documentation verbatim.
5. **No provider logos/trademarks** in the adapter.

The unbranded, generic adapter scaffold (`stunt adapter new`) is unaffected by these rules.

## Adapters in this repo

| Directory | Simulates | Backing |
|---|---|---|
| `adapters/stripe-style` | a Stripe-style payments API (charges, customers, balance, events) | Collection + Starlark |
| `adapters/drive-style` | a Google-Drive-style files API (files, folders, about/quota) | Blob + Collection |
| `adapters/twitter-style` | an X.com / Twitter-style API (auth, tweets, users, timeline) | Identity + Collection (pure-mock) |
| `adapters/dropbox-style` | a Dropbox-style files API (upload/download, list_folder, metadata) | Blob + Collection |

Each adapter is **broader than a minimal demo** but remains an MVP: enough endpoints to be a useful
local stand-in and to exercise the stunt primitives end to end. See each adapter's own README.

## Using an adapter

Point a `stunt.yaml` service at the adapter directory (local path for now; git refs via the catalog
later):

```yaml
services:
  stripe:
    adapter: ./adapters/stripe-style
```

Then `stunt up` serves it (port mode by default; `mode: subdomain` for `https://stripe.localhost`).
