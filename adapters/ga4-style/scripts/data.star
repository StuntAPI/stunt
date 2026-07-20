# GA4 Data API handlers — runReport and runRealtimeReport.
#
# POST /v1beta/properties/{property}:runReport
# POST /v1beta/properties/{property}:runRealtimeReport
#
# The body specifies dateRanges, dimensions, metrics, limit, offset, and
# optional dimensionFilter. The response includes dimensionHeaders,
# metricHeaders, rows (each with dimensionValues + metricValues), rowCount,
# and metadata.
#
# This mock produces DETERMINISTIC report data from a small set of synthetic
# dimensions (date, country, deviceCategory) and metrics (sessions,
# activeUsers, screenPageViews).

# Shared helpers (_bearer, _require_bearer, _to_int) are preloaded from
# scripts/lib.star.

# --- dimension/metric definitions ---

_VALID_DIMENSIONS = ["date", "country", "deviceCategory"]

_VALID_METRICS = ["sessions", "activeUsers", "screenPageViews"]

# Deterministic synthetic data rows keyed by dimension combination.
# Each entry is a {dimension_value: metric_value} map.
_DATA = {
    "date": {
        "20240101": {"sessions": "1200", "activeUsers": "900", "screenPageViews": "3500"},
        "20240102": {"sessions": "1350", "activeUsers": "1020", "screenPageViews": "3900"},
        "20240103": {"sessions": "980", "activeUsers": "740", "screenPageViews": "2800"},
        "20240104": {"sessions": "1500", "activeUsers": "1120", "screenPageViews": "4200"},
        "20240105": {"sessions": "1800", "activeUsers": "1350", "screenPageViews": "5100"},
        "20240106": {"sessions": "2100", "activeUsers": "1580", "screenPageViews": "6200"},
        "20240107": {"sessions": "1950", "activeUsers": "1470", "screenPageViews": "5800"},
    },
    "country": {
        "United States": {"sessions": "5200", "activeUsers": "3900", "screenPageViews": "15400"},
        "United Kingdom": {"sessions": "1800", "activeUsers": "1350", "screenPageViews": "5300"},
        "Germany": {"sessions": "1200", "activeUsers": "900", "screenPageViews": "3600"},
        "Japan": {"sessions": "900", "activeUsers": "680", "screenPageViews": "2700"},
    },
    "deviceCategory": {
        "mobile": {"sessions": "6800", "activeUsers": "5100", "screenPageViews": "18200"},
        "desktop": {"sessions": "3500", "activeUsers": "2630", "screenPageViews": "12300"},
        "tablet": {"sessions": "800", "activeUsers": "600", "screenPageViews": "2400"},
    },
}

# The key ordering for each dimension.
_DATA_KEYS = {
    "date": ["20240101", "20240102", "20240103", "20240104", "20240105", "20240106", "20240107"],
    "country": ["United States", "United Kingdom", "Germany", "Japan"],
    "deviceCategory": ["mobile", "desktop", "tablet"],
}

# on_data dispatches to runReport or runRealtimeReport based on the
# resource verb suffix (the Google API convention: properties/<id>:runReport).
def on_data(req):
    resource = req["params"].get("resource", "")
    if _contains(resource, ":runRealtimeReport"):
        return _run_realtime_report(req)
    return _run_report(req)

# _run_report produces a deterministic report.
# POST /v1beta/properties/{resource}:runReport (Bearer)
def _run_report(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    dimensions = _extract_dimension_names(body.get("dimensions", []))
    metrics = _extract_metric_names(body.get("metrics", []))
    limit = _to_int(str(body.get("limit", "10000")))
    if limit == 0:
        limit = 10000
    offset = _to_int(str(body.get("offset", "0")))

    # Build dimension headers and metric headers.
    dimension_headers = []
    for d in dimensions:
        dimension_headers.append({"name": d})
    metric_headers = []
    for m in metrics:
        metric_headers.append({"name": m, "type": "TYPE_INTEGER"})

    # Build rows from deterministic data.
    rows = _build_rows(dimensions, metrics, limit, offset)
    total_row_count = _total_row_count(dimensions)

    return respond(200, {
        "dimensionHeaders": dimension_headers,
        "metricHeaders": metric_headers,
        "rows": rows,
        "rowCount": total_row_count,
        "metadata": {
            "dataLossFromOtherRow": False,
            "samplingMetadatas": [],
            "subjectToThresholding": False,
            "currencyCode": "USD",
            "timeZone": "America/Los_Angeles",
        },
        "kind": "analyticsData#runReport",
    })

# _run_realtime_report produces a deterministic realtime report.
# POST /v1beta/properties/{resource}:runRealtimeReport (Bearer)
def _run_realtime_report(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    dimensions = _extract_dimension_names(body.get("dimensions", []))
    metrics = _extract_metric_names(body.get("metrics", []))

    # Realtime reports use smaller numbers.
    dimension_headers = []
    for d in dimensions:
        dimension_headers.append({"name": d})
    metric_headers = []
    for m in metrics:
        metric_headers.append({"name": m, "type": "TYPE_INTEGER"})

    # Build a smaller set of rows for realtime.
    rows = _build_realtime_rows(dimensions, metrics)

    return respond(200, {
        "dimensionHeaders": dimension_headers,
        "metricHeaders": metric_headers,
        "rows": rows,
        "rowCount": len(rows),
        "kind": "analyticsData#runRealtimeReport",
    })

# --- helpers ---

# _extract_dimension_names pulls the "name" field from each dimension spec.
def _extract_dimension_names(dims):
    out = []
    for d in dims:
        name = d.get("name", "")
        if name != "":
            out.append(name)
    return out

# _extract_metric_names pulls the "name" field from each metric spec.
def _extract_metric_names(mets):
    out = []
    for m in mets:
        name = m.get("name", "")
        if name != "":
            out.append(name)
    return out

# _build_rows generates report rows from the deterministic data.
def _build_rows(dimensions, metrics, limit, offset):
    if len(dimensions) == 0:
        # No dimensions → single aggregate row.
        return [_aggregate_row(metrics)]

    # Use the first dimension as the primary grouping.
    primary = dimensions[0]
    keys = _DATA_KEYS.get(primary, [])
    rows = []
    for key in keys:
        dim_values = {}
        for d in dimensions:
            if d == primary:
                dim_values[d] = key
            else:
                dim_values[d] = "All"
        metric_data = _DATA.get(primary, {}).get(key, {})
        metric_values = {}
        for m in metrics:
            metric_values[m] = metric_data.get(m, "0")
        rows.append(_make_row(dimensions, metrics, dim_values, metric_values))

    # Apply offset + limit.
    end = offset + limit
    if end > len(rows):
        end = len(rows)
    if offset > len(rows):
        offset = len(rows)
    return rows[offset:end]

def _build_realtime_rows(dimensions, metrics):
    if len(dimensions) == 0:
        return [_make_row(dimensions, metrics, {}, {
            "sessions": "42",
            "activeUsers": "28",
            "screenPageViews": "95",
        })]

    primary = dimensions[0]
    keys = _DATA_KEYS.get(primary, [])
    # Take only the first 2 for realtime.
    rows = []
    for key in keys[:2]:
        dim_values = {}
        for d in dimensions:
            if d == primary:
                dim_values[d] = key
            else:
                dim_values[d] = "All"
        rows.append(_make_row(dimensions, metrics, dim_values, {
            "sessions": "15",
            "activeUsers": "12",
            "screenPageViews": "38",
        }))
    return rows

def _aggregate_row(metrics):
    metric_values = {}
    totals = {"sessions": "15630", "activeUsers": "11760", "screenPageViews": "45300"}
    for m in metrics:
        metric_values[m] = totals.get(m, "0")
    row = {"dimensionValues": [], "metricValues": []}
    for m in metrics:
        row["metricValues"].append({"value": metric_values[m]})
    return row

def _make_row(dimensions, metrics, dim_values, metric_values):
    row = {"dimensionValues": [], "metricValues": []}
    for d in dimensions:
        row["dimensionValues"].append({"value": dim_values.get(d, "")})
    for m in metrics:
        row["metricValues"].append({"value": metric_values.get(m, "0")})
    return row

def _total_row_count(dimensions):
    if len(dimensions) == 0:
        return 1
    primary = dimensions[0]
    return len(_DATA_KEYS.get(primary, []))
