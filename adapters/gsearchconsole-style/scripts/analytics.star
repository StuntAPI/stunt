# Search analytics handler — Google Search Console API.
#
# POST /webmasters/v3/sites/{siteUrl}/searchAnalytics/query
#   Body: {startDate, endDate, dimensions, dimensionFilterGroups,
#          aggregationType, rowLimit, startRow, dataState}
#   → {rows, responseAverages, responseAggregationType}
#
# Rows are DERIVED from the synthetic query store (dates × queries × pages ×
# devices) at request time: the requested dimensions form the row keys, the
# metrics (clicks/impressions/ctr/position) are aggregated per row from the
# underlying facts, and the date range + dimension filters are honored
# before rowLimit/startRow slice the (clicks-desc) sorted rows.

# The synthetic query store: search queries, site page paths, and devices.
# Metric values for each (date, query, page, device) fact are derived
# deterministically from a stable hash, so the same request always returns
# the same numbers.
_QUERIES = ["how to tie a tie", "best running shoes", "python tutorial", "weather forecast", "recipe ideas"]
_PAGES = ["/guide/tie", "/products/shoes", "/docs/python", "/weather", "/recipes"]
_DEVICES = ["MOBILE", "DESKTOP", "TABLET"]

# Dimensions this simulator models (the real API also accepts country /
# searchAppearance / hour, which the synthetic store does not carry).
_MODELED_DIMS = ["date", "query", "page", "device"]

# _MAX_DAYS bounds the derived fact space (the real API returns at most ~16
# months of history; this simulator derives at most one quarter per request,
# and shrinks the window further for queries that group at fine granularity
# so every response stays inside the sandbox's VM step budget). Longer
# requested ranges are derived over their MOST RECENT window.
_MAX_DAYS = 90

# _MAX_GROUPS bounds the number of day × group cells derived per request
# (measured against the sandbox's per-call VM step budget).
_MAX_GROUPS = 1800

# on_query derives search analytics rows for the property.
def on_query(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    site, err = _require_site(req)
    if err != None:
        return err

    body = _body_of(req)
    if body == None:
        return _invalid_argument("Request body is not a valid JSON object.")

    # --- validate the date range (required, YYYY-MM-DD, start <= end) ---
    start_date = body.get("startDate", "")
    end_date = body.get("endDate", "")
    if start_date == None:
        start_date = ""
    if end_date == None:
        end_date = ""
    start_ord = _day_ordinal(start_date)
    end_ord = _day_ordinal(end_date)
    if start_ord == 0 or end_ord == 0:
        return _invalid_argument("startDate and endDate are required YYYY-MM-DD dates.")
    if end_ord < start_ord:
        return _invalid_argument("Invalid date range: endDate is before startDate.")
    if end_ord - start_ord + 1 > _MAX_DAYS:
        start_ord = end_ord - _MAX_DAYS + 1

    # --- validate dimensions ---
    dimensions = body.get("dimensions", [])
    if dimensions == None:
        dimensions = []
    seen = {}
    for d in dimensions:
        if d not in _MODELED_DIMS:
            return _invalid_argument("Invalid dimension: " + str(d))
        if d in seen:
            return _invalid_argument("Duplicate dimension: " + str(d))
        seen[d] = True

    # --- validate + normalize dimension filter groups ---
    groups = _filter_groups(body.get("dimensionFilterGroups", None))
    if type(groups) == str:
        return _invalid_argument(groups)
    for g in groups:
        bad = _validate_group(g)
        if bad != None:
            return _invalid_argument(bad)

    # --- rowLimit / startRow (real defaults and caps) ---
    row_limit = _body_int(body, "rowLimit", 1000)
    if row_limit <= 0:
        row_limit = 1000
    if row_limit > 25 * 1000:
        return _invalid_argument("rowLimit must be between 1 and " + str(25 * 1000) + ".")
    start_row = _body_int(body, "startRow", 0)
    if start_row < 0:
        return _invalid_argument("startRow cannot be negative.")

    aggregation_type = body.get("aggregationType", "auto")
    if aggregation_type == None:
        aggregation_type = "auto"

    # --- derive rows over the fact space, aggregating as we go ---
    # Facts (dates × queries × pages × devices) are grouped into units by
    # their projected row key, so per-day grouping work is per-UNIT, not
    # per-fact, and each fact's metrics derive from a precomputed base hash
    # mixed with the day ordinal — keeping multi-month ranges inside the
    # sandbox's per-call VM step budget.
    origin = _site_origin(site.get("siteUrl", ""))
    host = _site_host(site.get("siteUrl", ""))
    pages = []
    for p in _PAGES:
        pages.append(origin + p)

    date_pos = -1
    for i in range(len(dimensions)):
        if dimensions[i] == "date":
            date_pos = i

    mod = 1024 * 1024
    n_groups = len(groups)
    # Facts are grouped into units by their projected row key (one unit per
    # distinct value tuple of the non-date dimensions), via a one-time dict
    # pass — not adjacency, since page varies inside the loop nesting.
    units_by_key = {}
    unit_order = []
    for qi in range(len(_QUERIES)):
        for pi in range(len(pages)):
            for di in range(len(_DEVICES)):
                query = _QUERIES[qi]
                page = pages[pi]
                device = _DEVICES[di]
                base = (_stable_hash(host + "|" + query) + _stable_hash(host + page) * 61 + _stable_hash(host + "#" + device) * 991) % mod
                fact = [base, query, page, device]
                key = ""
                keys = []
                for d in dimensions:
                    if d == "query":
                        key = key + query + "\x1f"
                        keys.append(query)
                    elif d == "page":
                        key = key + page + "\x1f"
                        keys.append(page)
                    elif d == "device":
                        key = key + device + "\x1f"
                        keys.append(device)
                u = units_by_key.get(key, None)
                if u == None:
                    units_by_key[key] = [keys, [fact]]
                    unit_order.append(key)
                else:
                    u[1].append(fact)
    units = []
    for key in unit_order:
        units.append(units_by_key[key])

    groups_by_key = {}
    tot_clicks = 0
    tot_impressions = 0
    tot_pos_num = 0    # sum(position*100 * impressions)

    # Keep day × group cells inside the derivation budget: narrow the window
    # to its most recent days when the requested dimensions are fine-grained.
    n_units = len(units)
    if n_units > 0:
        budget_days = _MAX_GROUPS // n_units
        if budget_days < 1:
            budget_days = 1
        if end_ord - start_ord + 1 > budget_days:
            start_ord = end_ord - budget_days + 1

    for day in range(start_ord, end_ord + 1):
        date = _ordinal_day(day)
        day_mix = (day * 7919) % mod
        # Groups are keyed by small ints (day ordinal × unit index): hashing
        # a 60-char value string per dict op would dominate the step budget.
        day_base = day * 1000
        for ui in range(len(units)):
            u = units[ui]
            u_clicks = 0
            u_impressions = 0
            u_pos = 0
            for fi in range(len(u[1])):
                c = u[1][fi]
                if n_groups > 0 and not _fact_matches(date, c[1], c[2], c[3], groups):
                    continue
                h = (c[0] + day_mix) % mod
                clicks = 1 + h % 137
                impressions = clicks + 20 + ((h // 137) % 613)
                pos100 = 105 + ((h // 997) % 880)
                u_clicks = u_clicks + clicks
                u_impressions = u_impressions + impressions
                u_pos = u_pos + pos100 * impressions
            if u_impressions == 0:
                continue
            tot_clicks = tot_clicks + u_clicks
            tot_impressions = tot_impressions + u_impressions
            tot_pos_num = tot_pos_num + u_pos
            # Without a date dimension the row aggregates the whole range, so
            # the unit identity alone keys the group; with date, each day is
            # its own row.
            if date_pos < 0:
                key = ui
            else:
                key = day_base + ui
            g = groups_by_key.get(key, None)
            if g == None:
                g = {"keys": _insert_date_key(dimensions, date, u[0]), "clicks": 0, "impressions": 0, "pos_num": 0}
                groups_by_key[key] = g
            g["clicks"] = g["clicks"] + u_clicks
            g["impressions"] = g["impressions"] + u_impressions
            g["pos_num"] = g["pos_num"] + u_pos

    # --- materialize, sort by clicks desc, then startRow/rowLimit ---
    rows = []
    for key in groups_by_key:
        g = groups_by_key[key]
        rows.append({
            "keys": g["keys"],
            "clicks": float(g["clicks"]),
            "impressions": float(g["impressions"]),
            "ctr": float(g["clicks"]) / float(g["impressions"]),
            "position": float(g["pos_num"]) / (float(g["impressions"]) * 100.0),
        })
    total_rows = len(rows)
    rows = query_select(rows, None, "clicks", "desc", row_limit, start_row, None)

    out = {
        "responseAggregationType": aggregation_type,
        "responseAverages": _averages(tot_clicks, tot_impressions, tot_pos_num),
    }
    if total_rows > 0:
        out["rows"] = rows
    return respond(200, out)

# --- fact model ---

# _insert_date_key builds the public keys list for a new group: the unit's
# non-date key values with the date slotted in at its requested position.
def _insert_date_key(dimensions, date, keys_without_date):
    out = []
    ki = 0
    for d in dimensions:
        if d == "date":
            out.append(date)
        else:
            out.append(keys_without_date[ki])
            ki = ki + 1
    return out

# _averages builds the responseAverages object for the aggregated totals.
def _averages(clicks, impressions, pos_num):
    if impressions == 0:
        return {"clicks": 0.0, "impressions": 0.0, "ctr": 0.0, "position": 0.0}
    return {
        "clicks": float(clicks),
        "impressions": float(impressions),
        "ctr": float(clicks) / float(impressions),
        "position": float(pos_num) / (float(impressions) * 100.0),
    }

# --- dimension filters ---

# _filter_groups normalizes dimensionFilterGroups to a list; returns a string
# describing the problem when the shape is invalid.
def _filter_groups(raw):
    if raw == None:
        return []
    if type(raw) != "list":
        return "dimensionFilterGroups must be a list."
    return raw

# _validate_group checks one filter group; returns a message or None.
def _validate_group(group):
    if type(group) != "dict":
        return "Each dimension filter group must be an object."
    filters = group.get("filters", [])
    if filters == None:
        return None
    if type(filters) != "list":
        return "dimensionFilterGroups.filters must be a list."
    for f in filters:
        if type(f) != "dict":
            return "Each dimension filter must be an object."
        dim = f.get("dimension", "")
        if dim == None or dim == "":
            return "dimensionFilterGroups.filters.dimension is required."
        if dim not in _MODELED_DIMS:
            return "Invalid dimension: " + str(dim)
        op = f.get("operator", "equals")
        if op == None:
            op = "equals"
        if op not in ["equals", "notEquals", "contains", "notContains", "includingRegex", "excludingRegex"]:
            return "Invalid filter operator: " + str(op)
        expr = f.get("expression", "")
        if expr == None or expr == "":
            return "dimensionFilterGroups.filters.expression is required."
    return None

# _fact_matches evaluates the filter groups against one fact: groups are
# AND'ed; within a group, "and" (default) requires every filter to match and
# "or" requires at least one.
def _fact_matches(date, query, page, device, groups):
    for g in groups:
        filters = g.get("filters", [])
        if filters == None:
            continue
        group_type = g.get("groupType", "and")
        if group_type == None:
            group_type = "and"
        acc = group_type != "or"
        for f in filters:
            ok = _filter_matches(date, query, page, device, f)
            if group_type == "or":
                acc = acc or ok
            else:
                acc = acc and ok
        if not acc:
            return False
    return True

# _filter_matches evaluates a single dimension filter against a fact.
def _filter_matches(date, query, page, device, f):
    dim = f.get("dimension", "")
    op = f.get("operator", "equals")
    if op == None:
        op = "equals"
    expr = f.get("expression", "")
    if expr == None:
        expr = ""
    value = date
    if dim == "query":
        value = query
    elif dim == "page":
        value = page
    elif dim == "device":
        value = device
    needle = expr.lower()
    hay = value.lower()
    if op == "equals" or op == "includingRegex":
        return hay == needle
    if op == "notEquals":
        return hay != needle
    if op == "contains":
        return _contains(hay, needle)
    if op == "notContains":
        return not _contains(hay, needle)
    if op == "excludingRegex":
        return hay != needle
    return True

# _body_int reads an int from the JSON body (accepts int/float/string forms).
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
