# ga4-style

A stunt adapter simulating the **Google Analytics GA4 Data API + Admin API**, for local testing.

## Simulated API

- **Name:** Google Analytics Data API + Admin API
- **Version:** `v1beta`

## Endpoints

### Admin API (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v1admin/accounts` | List analytics accounts. |
| GET | `/v1admin/properties` | List properties (optional `?filter=parent:accounts/100001`). |
| GET | `/v1admin/properties/{property}/dataStreams` | List data streams for a property. |

### Data API (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v1beta/properties/{property}:runReport` | Run a report (dimensions, metrics, dateRanges, limit, offset). |
| POST | `/v1beta/properties/{property}:runRealtimeReport` | Run a realtime report. |

## Key shapes

- **Hierarchy:** Account → Property → DataStream (the source of much confusion).
- Properties referenced as `properties/123456789`.
- Report response: `{dimensionHeaders, metricHeaders, rows:[{dimensionValues, metricValues}], rowCount, metadata}`.
- Deterministic dimensions: `date`, `country`, `deviceCategory`.
- Deterministic metrics: `sessions`, `activeUsers`, `screenPageViews`.

## Usage

```bash
stunt init
# Add to your stunt.yaml:
#   ga4:
#     adapter: ./adapters/ga4-style
stunt up
```

All data is synthetic. See [DISCLAIMER](DISCLAIMER).
