# Shared library for anaplan-style adapter scripts.

# _check_auth validates Anaplan auth. Accepts either Basic auth
# (email:password) or Bearer token.
def _check_auth(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:6] == "Basic ":
        return auth[6:]
    if auth[:7] == "Bearer ":
        return auth[7:]
    return None

# _require_auth returns (token, None) if auth is present, or
# (None, error_response) if missing.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, respond(401, {
            "status": "FAILURE",
            "statusMessage": "Authentication is required. Provide Basic or Bearer credentials.",
        })
    return token, None

# _to_int parses a decimal string to int.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _num coerces a JSON-round-tripped number (int or float) to int. Sizes and
# offsets stored in a collection come back as floats.
def _num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(str(v))

# _BLOB_NS is the blob namespace holding uploaded file contents (and export
# output) — the byte-exact upload side of the file cycle.
_BLOB_NS = "anaplan_files"

# _file_key scopes a file (or catalog entity) id to its workspace + model, the
# unit of Anaplan's model-level data. Used as the files/tasks collection doc
# id (and scoping prefix for catalog lists).
def _file_key(ws, mid, fid):
    return ws + ":" + mid + ":" + fid

# _blob_key is the blob-store form of _file_key: the blob store rejects ":"
# in names, so the same triple is joined with "_".
def _blob_key(ws, mid, fid):
    return ws + "_" + mid + "_" + fid

# _csv_rows parses CSV content into a list of row dicts keyed by the header
# line's column names. Returns [] when there is no data row. A trailing
# newline and \r\n line endings are tolerated.
def _csv_rows(content):
    if content == None or content == "":
        return []
    lines = []
    for ln in content.split("\n"):
        ln = ln.strip("\r")
        if ln != "":
            lines.append(ln)
    if len(lines) <= 1:
        return []
    header = lines[0].split(",")
    rows = []
    for i in range(1, len(lines)):
        vals = lines[i].split(",")
        row = {}
        for j in range(len(header)):
            if j < len(vals):
                row[header[j]] = vals[j]
            else:
                row[header[j]] = ""
        rows.append(row)
    return rows

# _csv_header returns the column names from the first line of CSV content.
def _csv_header(content):
    if content == None or content == "":
        return []
    lines = content.split("\n")
    return lines[0].strip("\r").split(",")

# _csv_render renders rows (list of dicts) back into CSV text using the given
# column order — dict iteration order does not survive the JSON round trip,
# so the model data doc stores the header columns alongside the rows.
def _csv_render(rows, columns):
    if len(rows) == 0 or len(columns) == 0:
        return ""
    out = ",".join(columns)
    for r in rows:
        vals = []
        for k in columns:
            vals.append(str(r.get(k, "")))
        out = out + "\n" + ",".join(vals)
    return out

# _seeded_models lists the seeded (workspace, model) pairs the catalog is
# provisioned for. Mirrors the _MODELS table in scripts/models.star.
_SEEDED_MODELS = [
    ["8a819c8645a0aa8e0005c715c7ad49b9", "A101"],
    ["8a819c8645a0aa8e0005c715c7ad49b9", "A102"],
    ["8a819c8645b1bb9f0006c825d8be50c0", "B201"],
]

# _seed_catalog provisions the model-level action catalog (imports, exports,
# actions, processes) plus a default uploaded file for the primary import, so
# a bare import run has content to apply. File ids are assembled from short
# chunks (Anaplan file ids are long numerics).
def _seed_catalog():
    if store_kv_get("anaplan", "catalog_seeded") == "yes":
        return
    store_kv_set("anaplan", "catalog_seeded", "yes")

    import_file_id = "113" + "000" + "001"
    export_file_id = "114" + "000" + "002"
    export_file_id_2 = "114" + "000" + "003"

    imps = store_collection("imports")
    exps = store_collection("exports")
    acts = store_collection("actions")
    procs = store_collection("processes")
    files = store_collection("files")
    b = store_blob(_BLOB_NS)

    default_csv = "Region,Product,Revenue\n" + \
        "North,Widget,1200\n" + \
        "South,Gadget,850\n" + \
        "East,Gizmo,430\n"

    for pair in _SEEDED_MODELS:
        ws = pair[0]
        mid = pair[1]

        imps.insert({
            "id": _file_key(ws, mid, "imp001"),
            "entityId": "imp001",
            "name": "Load Revenue Data",
            "type": "IMPORT",
            "fileType": "CSV",
            "fileId": import_file_id,
        })
        imps.insert({
            "id": _file_key(ws, mid, "imp002"),
            "entityId": "imp002",
            "name": "Load Headcount Data",
            "type": "IMPORT",
            "fileType": "CSV",
            "fileId": import_file_id,
        })
        exps.insert({
            "id": _file_key(ws, mid, "exp001"),
            "entityId": "exp001",
            "name": "Revenue Export",
            "type": "FLAT",
            "format": "CSV",
            "fileId": export_file_id,
        })
        exps.insert({
            "id": _file_key(ws, mid, "exp002"),
            "entityId": "exp002",
            "name": "Expense Export",
            "type": "FLAT",
            "format": "CSV",
            "fileId": export_file_id_2,
        })
        acts.insert({
            "id": _file_key(ws, mid, "act001"),
            "entityId": "act001",
            "name": "Copy Revenue Actuals",
            "type": "ACTION",
        })
        procs.insert({
            "id": _file_key(ws, mid, "proc001"),
            "entityId": "proc001",
            "name": "Monthly Close Process",
            "type": "PROCESS",
        })

        # Default upload for the import file (3 data rows), so an import run
        # without a prior upload still applies real content.
        b.put(_blob_key(ws, mid, import_file_id), default_csv, "text/csv")
        files.insert({
            "id": _file_key(ws, mid, import_file_id),
            "fileId": import_file_id,
            "name": "revenue_load.csv",
            "contentType": "text/csv",
            "size": len(default_csv),
            "chunks": [{"index": 0, "offset": 0, "size": len(default_csv)}],
        })

# _list_page slices a list of docs by the Anaplan pagination query params
# (limit = page size, offset = opaque cursor token) via the paginate() builtin
# and returns (page, next_cursor). A missing/empty limit disables paging.
# next_cursor is None when no items remain.
def _list_page(req, docs):
    q = req.get("query")
    if q == None:
        q = {}
    limit = _to_int(q.get("limit", ""))
    cursor = q.get("offset", "")
    return paginate(docs, limit, cursor)

# _seed populates default workspaces.
def _seed():
    if store_kv_get("anaplan", "seeded") == "yes":
        return
    store_kv_set("anaplan", "seeded", "yes")

    wc = store_collection("workspaces")
    wc.insert({
        "id": "8a819c8645a0aa8e0005c715c7ad49b9",
        "name": "Supply Chain Planning",
        "active": True,
        "size": 1048576,
    })
    wc.insert({
        "id": "8a819c8645b1bb9f0006c825d8be50c0",
        "name": "Financial Forecasting",
        "active": True,
        "size": 2097152,
    })
