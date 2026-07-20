# gsearchconsole-style

A stunt adapter simulating the **Google Search Console API** with deterministic
seeded search analytics, for local testing.

## Simulated API

- **Name:** Google Search Console API
- **Version:** `v1`

## Why this adapter?

Google Search Console's search analytics query returns rows keyed by dimensions
(query, page, country, etc.) with clicks, impressions, CTR, and position metrics.
The URL inspection API is also commonly needed for verifying index status. This
adapter provides deterministic seeded data so you can test your analytics pipeline
locally.

## Auth

- **Bearer:** `Authorization: Bearer <oauth2-token>`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/webmasters/v3/sites/{siteUrl}/searchAnalytics/query` | Query search analytics (`{startDate, endDate, dimensions, rowLimit}`). |
| GET | `/webmasters/v3/sites` | List verified sites. |
| GET | `/webmasters/v3/sites/{siteUrl}/sitemaps` | List sitemaps. |
| POST | `/webmasters/v3/sites/{siteUrl}/inspect` | Inspect a URL. |

## Key shapes

- Search analytics row: `{keys:["query","page"], clicks, impressions, ctr, position}`.
- Search analytics response: `{rows:[...], responseAggregationType}`.
- Sites: `{siteEntry:[{siteUrl, permissionLevel}]}`.
- Sitemaps: `{sitemap:[{path, lastSubmitted, errors, contents}]}`.
- Inspection: `{inspectionResult:{indexStatusResult:{verdict, coverageState, ...}}}`.

## Data model

Sites are **stateful**. Two sample sites are seeded. Search analytics rows are
deterministic (seeded by index) for reproducible testing.
