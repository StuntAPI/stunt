# Shared library for fattureincloud-style adapter scripts.
#
# Preloaded by stunt before each handler in this directory (see
# internal/starlark/vm.go LoadWithLib). Models the v2 conventions the real
# API is known for:
#
#   - single resources wrapped in {"data": {...}}
#   - lists in a Laravel-style pagination envelope
#   - amounts as DECIMAL STRINGS ("9800.00") — parse, don't cast
#   - company ids in the path; a foreign or unknown id is a plain 404

# _require_auth checks the Bearer token. For frictionless local testing any
# non-empty bearer is accepted; a missing header is a real 401 with the v2
# error shape.
def _require_auth(req):
    headers = req.get("headers", {})
    auth = headers.get("Authorization", "") or headers.get("authorization", "")
    if not auth.startswith("Bearer ") or len(auth) <= 7:
        return _api_error(401, "unauthorized", "The access token is missing or invalid.")
    return None

def _api_error(status, code, message):
    return respond(status, {"error": {"code": code, "message": message}})

# _to_int parses a decimal string to int with a fallback. No try/except in
# Starlark, and strings are not iterable here — hence isdigit().
def _to_int(s, fallback):
    t = str(s)
    if t.startswith("-"):
        t = t[1:]
    if t == "" or not t.isdigit():
        return fallback
    return int(t)

def _q1(query, name, default=""):
    # One query param as a string — single-element lists are flattened.
    v = query.get(name, default)
    if type(v) == "list":
        return str(v[0]) if len(v) > 0 else default
    return str(v)

def _body_of(req):
    # Parsed JSON body, or an empty dict.
    b = req.get("body")
    if b == None:
        raw = req.get("raw_body", "")
        if raw == "":
            return {}
        return json.loads(raw)
    return b

def _next_id(resource):
    return str(store_kv_incr("fattureincloud", resource + "_seq"))

def _strip_internal(doc):
    # Drop bookkeeping fields before a document leaves the simulator.
    out = {}
    for k in doc:
        if k in ("company_id",):
            continue
        out[k] = doc[k]
    return out

def _companies():
    return store_collection("companies")

def _require_company(req):
    # The company row for {company_id} in the path, or a 404 response.
    company_id = str(req.get("params", {}).get("company_id", ""))
    for c in _companies().list():
        if str(c.get("id", "")) == company_id:
            return c, None
    return None, _api_error(404, "not_found", "Company not found.")

def _rows(resource, company_id):
    rows = []
    for d in store_collection(resource).list():
        if str(d.get("company_id", "")) == str(company_id):
            rows.append(d)
    return rows

def _by_date(rows):
    # Insertion sort by date — no list .sort here, and sorted() refuses our
    # kwargs. Oldest first, matching the real API's default ordering.
    out = []
    for r in rows:
        k = str(r.get("date", ""))
        placed = False
        for i in range(len(out)):
            if k <= str(out[i].get("date", "")):
                out.insert(i, r)
                placed = True
                break
        if not placed:
            out.append(r)
    return out

def _paginate(rows, req, path):
    # Laravel-style envelope: current_page / data / from / last_page / path /
    # per_page / to / total. Clients must follow last_page.
    per_page = min(200, max(1, _to_int(_q1(req.get("query", {}), "per_page", "50"), 50)))
    page = max(1, _to_int(_q1(req.get("query", {}), "page", "1"), 1))
    total = len(rows)
    last_page = max(1, (total + per_page - 1) // per_page)
    page = min(page, last_page)
    start = (page - 1) * per_page
    chunk = rows[start:start + per_page]
    return {
        "current_page": page,
        "data": chunk,
        "from": (start + 1) if total > 0 else None,
        "last_page": last_page,
        "path": path,
        "per_page": per_page,
        "to": min(start + per_page, total) if total > 0 else None,
        "total": total,
    }

def _doc_wanted(req):
    # Filters shared by the document-ish resources: q, type, date_start/date_end
    # (inclusive, lexicographic on YYYY-MM-DD).
    query = req.get("query", {})
    def wanted(doc):
        q = _q1(query, "q", "").lower()
        if q != "":
            entity = doc.get("entity", {})
            entity_name = entity.get("name", "") if type(entity) == "dict" else ""
            hay = " ".join([str(entity_name), str(doc.get("description", "")),
                            str(doc.get("name", "")), str(doc.get("code", "")),
                            str(doc.get("category", ""))]).lower()
            if q not in hay:
                return False
        t = _q1(query, "type", "")
        if t != "" and str(doc.get("type", "")) != t:
            return False
        ds = _q1(query, "date_start", "")
        de = _q1(query, "date_end", "")
        date = str(doc.get("date", ""))
        if ds != "" and date != "" and date < ds:
            return False
        if de != "" and date != "" and date > de:
            return False
        return True
    return wanted

# ── generic CRUD over a company-scoped resource ─────────────────────────────
# One definition in lib (not per-script): stunt preloads every handler script
# together, so same-named functions across scripts shadow each other.

def _crud_list(req, resource):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    wanted = _doc_wanted(req)
    rows = [_strip_internal(d) for d in _rows(resource, company.get("id")) if wanted(d)]
    rows = _by_date(rows)
    return respond(200, _paginate(rows, req, "/entities/" + str(company.get("id")) + "/" + resource))

def _crud_create(req, resource, defaults):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    body = _body_of(req)
    doc = {}
    for k in defaults:
        doc[k] = defaults[k]
    for k in body:
        doc[k] = body[k]
    doc["id"] = _next_id(resource)
    doc["company_id"] = str(company.get("id"))
    store_collection(resource).insert(doc)
    return respond(201, {"data": _strip_internal(doc)})

def _crud_get(req, resource, what):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    doc_id = str(req.get("params", {}).get("id", ""))
    for d in _rows(resource, company.get("id")):
        if str(d.get("id")) == doc_id:
            return respond(200, {"data": _strip_internal(d)})
    return _api_error(404, "not_found", what + " not found.")

def _crud_modify(req, resource, what):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    doc_id = str(req.get("params", {}).get("id", ""))
    body = _body_of(req)
    coll = store_collection(resource)
    for d in _rows(resource, company.get("id")):
        if str(d.get("id")) == doc_id:
            patch = {}
            for k in d:
                patch[k] = d[k]
            for k in body:
                patch[k] = body[k]
            coll.update(d.get("id"), patch)
            return respond(200, {"data": _strip_internal(coll.get(d.get("id")))})
    return _api_error(404, "not_found", what + " not found.")

def _crud_delete(req, resource, what):
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    doc_id = str(req.get("params", {}).get("id", ""))
    for d in _rows(resource, company.get("id")):
        if str(d.get("id")) == doc_id:
            store_collection(resource).delete(d.get("id"))
            return respond(200, {})
    return _api_error(404, "not_found", what + " not found.")

def _categories_in_use(req, resource):
    # The metodata endpoint: which categories exist for this company's documents.
    err = _require_auth(req)
    if err:
        return err
    company, err = _require_company(req)
    if err:
        return err
    seen = []
    for d in _rows(resource, company.get("id")):
        c = str(d.get("category", ""))
        if c != "" and c not in seen:
            seen.append(c)
    return respond(200, {"data": {"categories": sorted(seen), "currencies": ["EUR"]}})
