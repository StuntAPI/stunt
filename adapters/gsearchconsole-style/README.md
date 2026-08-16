# gsearchconsole-style

A stunt adapter simulating the **Google Search Console API** with derived
search analytics, URL inspection, and a real site/sitemap lifecycle, for
local testing.

## Simulated API

- **Name:** Google Search Console API
- **Version:** `v1`

## Why this adapter?

Google Search Console's search analytics query returns rows keyed by
dimensions (query, page, device, …) with clicks, impressions, CTR, and
position metrics, filtered by date range and dimension filters; the URL
Inspection API reports index status verdicts per URL; the sites/sitemaps
APIs manage properties. This adapter derives all of that from a synthetic
query store so you can test your analytics pipeline locally.

## Auth

- **Bearer:** `Authorization: Bearer <oauth2-token>` — token-store backed:
  the well-known static test token `mock-oauth2-token` is seeded on first
  request; any other token is rejected with 401 `UNAUTHENTICATED`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/webmasters/v3/sites/{siteUrl}/searchAnalytics/query` | Query search analytics (see below). |
| GET | `/webmasters/v3/sites` | List verified sites (`{siteEntry:[…]}`; `maxResults`/`pageToken`). |
| GET | `/webmasters/v3/sites/{siteUrl}` | Get one site entry (404 when unknown). |
| PUT | `/webmasters/v3/sites/{siteUrl}` | `sites.add` → 204. New properties start **unverified** (`permissionLevel: siteUnverifiedUser`) and complete verification ~2s later (derive-on-read lifecycle, below). |
| DELETE | `/webmasters/v3/sites/{siteUrl}` | `sites.delete` → 204 (404 when unknown; also removes its sitemaps). |
| GET | `/webmasters/v3/sites/{siteUrl}/sitemaps` | List sitemaps (`{sitemap:[…]}`). |
| GET | `/webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}` | Get one sitemap (404 when unknown). |
| PUT | `/webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}` | `sitemaps.submit` → 204, `lastSubmitted` stamped from the clock. |
| DELETE | `/webmasters/v3/sites/{siteUrl}/sitemaps/{feedpath}` | `sitemaps.delete` → 204. |
| POST | `/v1/urlInspection/index:inspect` | The real URL Inspection endpoint. Body `{inspectionUrl, siteUrl, languageCode}`. |
| POST | `/webmasters/v3/sites/{siteUrl}/inspect` | Legacy inspection route (same pipeline, `siteUrl` from the path). |

### Site verification lifecycle

`PUT /sites/{siteUrl}` registers the property as `siteUnverifiedUser`, like
the real API before ownership is proven. While unverified, site-scoped
endpoints answer **403 `PERMISSION_DENIED`** ("has not been verified").
Verification is derived from the clock: ~2 seconds after the add, the next
read observes and persists the transition to `siteFullUser`. Unknown
properties answer 403 ("You don't have access…").

### Search analytics

`POST …/searchAnalytics/query` body: `{startDate, endDate, dimensions,
dimensionFilterGroups, aggregationType, rowLimit, startRow, dataState}`.

- **Dimensions modeled:** `date`, `query`, `page`, `device` (others → 400
  `INVALID_ARGUMENT`). Rows derive from the synthetic fact space
  (dates × queries × pages × devices) for the property, aggregated per row:
  `clicks`/`impressions` are sums, `ctr = clicks/impressions`, `position`
  is the impression-weighted average — so drill-downs are consistent
  (e.g. `["query"]` rows sum to the `["query","device"]` breakdown).
- **Filters:** `dimensionFilterGroups` are AND'ed; within a group,
  `groupType: "and"|"or"`; operators `equals`, `notEquals`, `contains`,
  `notContains` (plus `includingRegex`/`excludingRegex`, treated as
  equals/not-equals — no regex engine in the sandbox).
- **Paging/order:** rows sort by clicks desc; `rowLimit` (default 1000, max
  25000) and `startRow` slice the sorted rows. `responseAverages` carries
  the aggregated clicks/impressions/ctr/position; `responseAggregationType`
  echoes `aggregationType` (default `auto`). Empty results omit `rows`,
  like the real API.
- **Determinism:** metrics derive from a stable hash of
  (property, query, page, device, date) — same request, same numbers.
- **Simulator bounds:** ranges derive at most 90 days and at most ~1,800
  day × group cells per request; longer/fine-grained requests derive the
  most recent window that fits (the real API's 16-month window is not
  modeled).

### URL inspection

The inspection derives deterministic verdicts from the inspected URL path:
default `PASS` / `Indexed`; paths containing `noindex`, `disallow`,
`canonical`, `soft404`, or `servererror` produce the matching real
coverage/robots/fetch states (`NEUTRAL`/`PARTIAL` verdicts). Errors match
the real API: missing `inspectionUrl` or a URL outside the property → 400
`INVALID_ARGUMENT`; unknown or unverified property → 403
`PERMISSION_DENIED`.

## Key shapes

- Search analytics row: `{keys:["query","page"], clicks, impressions, ctr, position}`.
- Search analytics response: `{rows:[…], responseAverages:{clicks, impressions, ctr, position}, responseAggregationType}`.
- Sites: `{siteEntry:[{siteUrl, permissionLevel}]}`.
- Sitemaps: `{sitemap:[{path, lastSubmitted, lastDownloaded, isPending, contents, errors, warnings}]}`.
- Inspection: `{inspectionResult:{inspectionResultLink, indexStatusResult:{verdict, coverageState, robotsTxtState, indexingState, lastCrawlTime, pageFetchState, googleCanonical, userCanonical, referringUrls, crawledAs, transportEncryptionStatus}, mobileUsabilityResult, richResultsResult, ampResult}}`.
- Errors: `{error:{code, message, status}}` with `INVALID_ARGUMENT` / `PERMISSION_DENIED` / `NOT_FOUND` / `UNAUTHENTICATED`.

## Data model

Sites and sitemaps are **stateful** (three seeded verified properties, one
seeded sitemap each). The `{siteUrl}` / `{feedpath}` path params are matched
after percent-decoding; because the engine routes on decoded paths,
`https://…` URL-prefix properties and full-URL feedpaths are addressed by
single-segment forms (use `sc-domain:` properties or the final feedpath
segment). All data is synthetic.
