# GA4 Data API handlers — runReport and runRealtimeReport.
#
# POST /v1beta/properties/{property}:runReport
# POST /v1beta/properties/{property}:runRealtimeReport
#
# The body specifies dateRanges, dimensions, metrics, limit, offset, and
# optional dimensionFilter/metricFilter/orderBys. The response includes
# dimensionHeaders, metricHeaders, rows (each with dimensionValues +
# metricValues), rowCount, and metadata.
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

    bad = _validate_request(body)
    if bad != None:
        return bad

    dimensions = _extract_dimension_names(body.get("dimensions", []))
    metrics = _extract_metric_names(body.get("metrics", []))
    limit = _body_int(body, "limit", 10000)
    if limit == 0:
        limit = 10000
    offset = _body_int(body, "offset", 0)

    # Build dimension headers and metric headers.
    dimension_headers = []
    for d in dimensions:
        dimension_headers.append({"name": d})
    metric_headers = []
    for m in metrics:
        metric_headers.append({"name": m, "type": "TYPE_INTEGER"})

    # Build rows from deterministic data as name-keyed dicts, then apply the
    # real report query clauses (dimensionFilter, metricFilter, orderBys)
    # before limit/offset paging, like the real Data API.
    row_dicts = _build_row_dicts(dimensions, metrics)
    row_dicts = _apply_report_query(body, row_dicts)
    total_row_count = len(row_dicts)
    row_dicts = query_select(row_dicts, None, "", "", limit, offset, None)

    rows = []
    for rd in row_dicts:
        rows.append(_dict_to_row(rd, dimensions, metrics))

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

    bad = _validate_request(body)
    if bad != None:
        return bad

    dimensions = _extract_dimension_names(body.get("dimensions", []))
    metrics = _extract_metric_names(body.get("metrics", []))
    limit = _body_int(body, "limit", 10000)
    if limit == 0:
        limit = 10000
    offset = _body_int(body, "offset", 0)

    # Realtime reports use smaller numbers.
    dimension_headers = []
    for d in dimensions:
        dimension_headers.append({"name": d})
    metric_headers = []
    for m in metrics:
        metric_headers.append({"name": m, "type": "TYPE_INTEGER"})

    # Build a smaller set of rows for realtime, then apply the same query
    # clauses the real realtime report honors (dimensionFilter,
    # metricFilter, orderBys) before limit/offset paging.
    row_dicts = _build_realtime_row_dicts(dimensions, metrics)
    row_dicts = _apply_report_query(body, row_dicts)
    total_row_count = len(row_dicts)
    row_dicts = query_select(row_dicts, None, "", "", limit, offset, None)

    rows = []
    for rd in row_dicts:
        rows.append(_dict_to_row(rd, dimensions, metrics))

    return respond(200, {
        "dimensionHeaders": dimension_headers,
        "metricHeaders": metric_headers,
        "rows": rows,
        "rowCount": total_row_count,
        "kind": "analyticsData#runRealtimeReport",
    })

# --- helpers ---

# _invalid_argument returns the GA4 400 status envelope for a bad request.
def _invalid_argument(msg):
    return respond(400, {
        "error": {
            "code": 400,
            "message": msg,
            "status": "INVALID_ARGUMENT",
        },
    })

# _validate_request 400s like the real Data API on unknown dimension/metric
# names in dimensions, metrics, dimensionFilter/metricFilter fieldNames, and
# orderBys. Returns the error response or None.
def _validate_request(body):
    for d in _extract_dimension_names(body.get("dimensions", [])):
        if d not in _VALID_DIMENSIONS:
            return _invalid_argument("Unknown dimension name: " + d)
    for m in _extract_metric_names(body.get("metrics", [])):
        if m not in _VALID_METRICS:
            return _invalid_argument("Unknown metric name: " + m)

    err = _validate_filter(body.get("dimensionFilter", None), _VALID_DIMENSIONS)
    if err != None:
        return err
    err = _validate_filter(body.get("metricFilter", None), _VALID_METRICS)
    if err != None:
        return err

    obs = body.get("orderBys", [])
    if obs == None:
        obs = []
    for ob in obs:
        d = ob.get("dimension", None)
        if d != None:
            name = d.get("dimensionName", "")
            if name != "" and name not in _VALID_DIMENSIONS:
                return _invalid_argument("Unknown dimension name: " + name)
            continue
        m = ob.get("metric", None)
        if m != None:
            name = m.get("metricName", "")
            if name != "" and name not in _VALID_METRICS:
                return _invalid_argument("Unknown metric name: " + name)
    return None

# _validate_filter walks a (possibly nested) FilterExpression with an
# explicit stack (no recursion in starlark-go) and 400s on an unknown leaf
# fieldName. Returns the error response or None.
def _validate_filter(expr, valid_names):
    if expr == None:
        return None
    stack = [expr]
    while len(stack) > 0:
        e = stack.pop()
        if e == None:
            continue
        leaf = e.get("filter", None)
        if leaf != None:
            field = leaf.get("fieldName", "")
            if field != "" and field not in valid_names:
                return _invalid_argument("Unknown field name: " + field)
            continue
        sub = e.get("andGroup", None)
        if sub == None:
            sub = e.get("orGroup", None)
        if sub != None:
            exprs = sub.get("expressions", [])
            if exprs != None:
                for x in exprs:
                    stack.append(x)
            continue
        sub = e.get("notExpression", None)
        if sub != None:
            stack.append(sub)
    return None

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

# _build_row_dicts generates report rows from the deterministic data as
# dicts keyed by dimension/metric name (the shape query_select filters and
# sorts). The conversion to the wire shape ({dimensionValues, metricValues})
# happens after the query is applied.
def _build_row_dicts(dimensions, metrics):
    if len(dimensions) == 0:
        # No dimensions → single aggregate row.
        row = {}
        totals = {"sessions": "15630", "activeUsers": "11760", "screenPageViews": "45300"}
        for m in metrics:
            row[m] = totals.get(m, "0")
        return [row]

    # Use the first dimension as the primary grouping.
    primary = dimensions[0]
    keys = _DATA_KEYS.get(primary, [])
    rows = []
    for key in keys:
        row = {}
        for d in dimensions:
            if d == primary:
                row[d] = key
            else:
                row[d] = "All"
        metric_data = _DATA.get(primary, {}).get(key, {})
        for m in metrics:
            row[m] = metric_data.get(m, "0")
        rows.append(row)
    return rows

# _build_realtime_row_dicts generates the smaller realtime dataset as
# name-keyed row dicts (same shape as _build_row_dicts, so the report query
# clauses apply unchanged).
def _build_realtime_row_dicts(dimensions, metrics):
    if len(dimensions) == 0:
        row = {}
        totals = {"sessions": "42", "activeUsers": "28", "screenPageViews": "95"}
        for m in metrics:
            row[m] = totals.get(m, "0")
        return [row]

    primary = dimensions[0]
    keys = _DATA_KEYS.get(primary, [])
    # Take only the first 2 for realtime.
    rows = []
    for key in keys[:2]:
        row = {}
        for d in dimensions:
            if d == primary:
                row[d] = key
            else:
                row[d] = "All"
        totals = {"sessions": "15", "activeUsers": "12", "screenPageViews": "38"}
        for m in metrics:
            row[m] = totals.get(m, "0")
        rows.append(row)
    return rows

# --- report query clauses (dimensionFilter / metricFilter / orderBys) ---

# _body_int reads an int from the JSON body. JSON numbers can arrive as
# floats (limit: 2 → 2.0); str(2.0) is not a decimal int for _to_int, so
# convert the value directly before falling back to the string path.
def _body_int(body, key, default):
    v = body.get(key, None)
    if v == None:
        return default
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    n = _to_int(str(v))
    if n == 0:
        return default
    return n

# _apply_report_query applies the body's dimensionFilter, metricFilter, and
# orderBys to name-keyed row dicts via query_select. Paging (limit/offset)
# is applied separately by the caller so rowCount can reflect the filtered
# total, like the real Data API.
def _apply_report_query(body, rows):
    dim_filter = body.get("dimensionFilter", None)
    if dim_filter == None:
        dim_filter = {}
    rows = _filter_rows(rows, dim_filter)

    met_filter = body.get("metricFilter", None)
    if met_filter == None:
        met_filter = {}
    rows = _filter_rows(rows, met_filter)

    order_bys = _order_bys(body.get("orderBys", []))
    # query_select sorts by one key; applying the orderBys last-to-first
    # composes them into a multi-key sort (each pass is stable).
    for i in range(len(order_bys) - 1, -1, -1):
        ob = order_bys[i]
        rows = query_select(rows, None, ob[0], ob[1], None, None, None)
    return rows

# _filter_rows evaluates a GA4 FilterExpression (leaf filter, andGroup,
# orGroup, notExpression — arbitrarily nested) against the row dicts.
# starlark-go forbids recursion, so the expression tree is walked with an
# explicit stack; each node evaluates to a per-row boolean list.
def _filter_rows(rows, expr):
    if expr == None:
        return rows
    matched = _expr_matches(expr, rows)
    out = []
    for i in range(len(rows)):
        if matched[i]:
            out.append(rows[i])
    return out

# _expr_matches computes the per-row boolean match list for a
# FilterExpression. Stack entries are ["expr", node], ["fold", op, count],
# and ["not"]; leaves push their boolean list, folds combine the top
# `count` lists.
def _expr_matches(expr, rows):
    n = len(rows)
    results = []
    stack = [["expr", expr]]
    while len(stack) > 0:
        top = stack.pop()
        if top[0] == "not":
            a = results.pop()
            out = []
            for i in range(n):
                out.append(not a[i])
            results.append(out)
            continue
        if top[0] == "fold":
            count = top[2]
            group = []
            for i in range(count):
                group.append(results.pop())
            acc = group[0]
            for i in range(1, count):
                acc = _combine_bools(acc, group[i], top[1])
            results.append(acc)
            continue

        e = top[1]
        leaf = e.get("filter", None)
        if leaf != None:
            results.append(_leaf_matches(leaf, rows, n))
            continue

        and_group = e.get("andGroup", None)
        if and_group != None:
            exprs = and_group.get("expressions", [])
            if exprs == None:
                exprs = []
            if len(exprs) == 0:
                results.append(_all_bools(n, True))
                continue
            stack.append(["fold", "and", len(exprs)])
            for sub in exprs:
                stack.append(["expr", sub])
            continue

        or_group = e.get("orGroup", None)
        if or_group != None:
            exprs = or_group.get("expressions", [])
            if exprs == None:
                exprs = []
            if len(exprs) == 0:
                results.append(_all_bools(n, False))
                continue
            stack.append(["fold", "or", len(exprs)])
            for sub in exprs:
                stack.append(["expr", sub])
            continue

        not_expr = e.get("notExpression", None)
        if not_expr != None:
            stack.append(["not"])
            stack.append(["expr", not_expr])
            continue

        # Empty/unknown expression matches everything.
        results.append(_all_bools(n, True))
    return results.pop()

def _combine_bools(a, b, op):
    out = []
    if op == "and":
        for i in range(len(a)):
            out.append(a[i] and b[i])
    else:
        for i in range(len(a)):
            out.append(a[i] or b[i])
    return out

def _all_bools(n, v):
    out = []
    for i in range(n):
        out.append(v)
    return out

# _leaf_matches computes the per-row boolean list for a single GA4 Filter
# (stringFilter, inListFilter, or numericFilter) on fieldName. query_select
# preserves row order, so the selected subsequence marks the matches.
def _leaf_matches(leaf, rows, n):
    field = leaf.get("fieldName", "")
    if field == "":
        return _all_bools(n, True)
    sf = leaf.get("stringFilter", None)
    if sf != None:
        cs = sf.get("caseSensitive", False)
        if cs == None:
            cs = False
        if cs == False:
            # GA4 stringFilter defaults to case-insensitive; query_select's
            # string ops are case-sensitive, so compare lowercased sides in
            # a manual scan.
            return _ci_string_bools(sf, field, rows, n)
    f = _leaf_triples(leaf, field)
    if len(f) == 0:
        return _all_bools(n, True)
    selected = query_select(rows, f)
    bools = []
    for i in range(n):
        bools.append(False)
    j = 0
    for i in range(n):
        if j < len(selected) and rows[i] == selected[j]:
            bools[i] = True
            j = j + 1
    return bools

# _ci_string_bools evaluates a case-INsensitive stringFilter against the row
# dicts: both the row value and the filter value are lowercased before the
# matchType comparison (EXACT is the default matchType).
def _ci_string_bools(sf, field, rows, n):
    value = sf.get("value", "")
    if value == None:
        value = ""
    needle = value.lower()
    match_type = sf.get("matchType", "EXACT")
    bools = []
    for i in range(n):
        rv = rows[i].get(field, "")
        if rv == None:
            rv = ""
        hay = rv.lower()
        ok = False
        if match_type == "CONTAINS":
            ok = needle in hay
        elif match_type == "STARTS_WITH":
            ok = hay.startswith(needle)
        elif match_type == "ENDS_WITH":
            ok = hay.endswith(needle)
        else:
            ok = hay == needle
        bools.append(ok)
    return bools

# _leaf_triples maps a GA4 Filter to query_select [field, op, value] triples.
# stringFilter matchType EXACT is the default.
def _leaf_triples(leaf, field):
    sf = leaf.get("stringFilter", None)
    if sf != None:
        value = sf.get("value", "")
        if value == None:
            value = ""
        match_type = sf.get("matchType", "EXACT")
        if match_type == "CONTAINS":
            return [[field, "contains", value]]
        if match_type == "STARTS_WITH":
            return [[field, "startswith", value]]
        if match_type == "ENDS_WITH":
            return [[field, "endswith", value]]
        return [[field, "=", value]]

    il = leaf.get("inListFilter", None)
    if il != None:
        values = il.get("values", [])
        if values == None:
            values = []
        if len(values) > 0:
            return [[field, "in", values]]
        return []

    nf = leaf.get("numericFilter", None)
    if nf != None:
        operation = nf.get("operation", "")
        val = nf.get("value", None)
        if val == None:
            val = {}
        num = val.get("int64Value", None)
        if num == None:
            num = val.get("doubleValue", None)
        if num == None:
            return []
        if operation == "EQUAL":
            return [[field, "=", str(num)]]
        if operation == "GREATER_THAN":
            return [[field, ">", str(num)]]
        if operation == "GREATER_THAN_OR_EQUAL":
            return [[field, ">=", str(num)]]
        if operation == "LESS_THAN":
            return [[field, "<", str(num)]]
        if operation == "LESS_THAN_OR_EQUAL":
            return [[field, "<=", str(num)]]
        return []

    return []

# _order_bys maps the body's orderBys to [field, "asc"/"desc"] pairs
# (dimension.dimensionName / metric.metricName).
def _order_bys(obs):
    out = []
    if obs == None:
        return out
    for ob in obs:
        d = ob.get("dimension", None)
        if d != None and d.get("dimensionName", "") != "":
            out.append([d["dimensionName"], _order_dir(ob)])
            continue
        m = ob.get("metric", None)
        if m != None and m.get("metricName", "") != "":
            out.append([m["metricName"], _order_dir(ob)])
    return out

def _order_dir(ob):
    if ob.get("desc", False):
        return "desc"
    return "asc"

# _dict_to_row converts a name-keyed row dict back to the wire shape.
def _dict_to_row(row, dimensions, metrics):
    dim_values = {}
    metric_values = {}
    for d in dimensions:
        dim_values[d] = row.get(d, "")
    for m in metrics:
        metric_values[m] = row.get(m, "0")
    return _make_row(dimensions, metrics, dim_values, metric_values)

def _make_row(dimensions, metrics, dim_values, metric_values):
    row = {"dimensionValues": [], "metricValues": []}
    for d in dimensions:
        row["dimensionValues"].append({"value": dim_values.get(d, "")})
    for m in metrics:
        row["metricValues"].append({"value": metric_values.get(m, "0")})
    return row
