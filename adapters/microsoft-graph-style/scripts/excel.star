# Microsoft Graph v1.0 — Excel (workbook) handlers.
#
# GET    /v1.0/me/drive/items/{id}/workbook/worksheets                → worksheets
# GET    /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows        → list rows (OData)
# POST   /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows        → add rows (201)
# POST   /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/add    → add rows (200, legacy)
# PATCH  /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/{i}    → update one row
# DELETE /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/{i}    → delete one row (204)
# GET    /v1.0/me/drive/items/{id}/workbook/tables/{name}/range       → table range
#
# Table rows are STATEFUL: every added row is persisted (per workbook item
# + table) and shows up in GET rows, in PATCH/DELETE by row index, and in
# the table's range. No workbook sessions are modeled — there is no
# Workbook-Session-Id to forget to close — so every "session-less" write
# persists immediately, exactly like real Graph without a session.

# The seeded workbook table: every workbook-bearing drive item starts with
# one table ("Table1") with this header and two data rows.
_TABLE_NAME = "Table1"
_TABLE_COLUMNS = ["Region", "Units", "Revenue"]
_TABLE_SEED_ROWS = [
    ["North", 120, 4800],
    ["South", 90, 3600],
]

# on_list_worksheets returns worksheets in a workbook.
# GET /v1.0/me/drive/items/{id}/workbook/worksheets (Bearer)
def on_list_worksheets(req):
    err = _require_bearer(req)
    if err != None:
        return err

    item_id = req["params"].get("id", "mock-item")

    worksheets = [
        {
            "id": "Sheet1",
            "name": "Sheet1",
            "position": 0,
            "visibility": "visible",
        },
        {
            "id": "Sheet2",
            "name": "Sheet2",
            "position": 1,
            "visibility": "visible",
        },
    ]

    # Pagination: $top = page size, $skip = plain numeric offset.
    base_url = "https://graph.microsoft.com/v1.0/me/drive/items/" + item_id + "/workbook/worksheets"
    top = _to_int(req["query"].get("$top", ""))
    page, next_cursor, ok = _list_page(worksheets, req["query"])
    if not ok:
        return _err("invalidRequest", 400, "$top and $skip must be non-negative integers.")

    envelope = {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#workbook/worksheets",
        "value": page,
    }
    if next_cursor != None and next_cursor != "":
        envelope["@odata.nextLink"] = _odata_link(base_url, top, next_cursor)
    return respond(200, envelope)

# on_list_rows returns the persisted data rows of a table.
# GET /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows (Bearer, OData)
def on_list_rows(req):
    err = _require_bearer(req)
    if err != None:
        return err

    doc = _load_table(req)
    if doc == None:
        return _table_not_found(req)

    entities = []
    idx = 0
    for row in doc.get("rows", []):
        entities.append({"index": idx, "values": row})
        idx = idx + 1

    base_url = _table_base_url(req) + "/rows"
    return _apply_odata(entities, req["query"], base_url)

# on_create_rows adds rows at the end (or at body.index) of a table — the
# current Graph route. Returns 201 with the first added workbookTableRow.
# POST /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows (Bearer)
def on_create_rows(req):
    return _add_rows(req, 201)

# on_add_table_row is the legacy rows/add spelling of the same add action;
# older clients still call it, and it keeps its 200 response.
# POST /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/add (Bearer)
# Body: { index?: int, values: [[v, ...], ...] }
def on_add_table_row(req):
    return _add_rows(req, 200)

# on_update_row replaces the values of one row (by 0-based index).
# PATCH /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/{index} (Bearer)
# Body: { values: [v, ...] } (a single flat row; a 1×N 2-D value is accepted too)
def on_update_row(req):
    err = _require_bearer(req)
    if err != None:
        return err

    doc = _load_table(req)
    if doc == None:
        return _table_not_found(req)

    row_index = _row_index(req)
    rows = doc.get("rows", [])
    if row_index < 0 or row_index >= len(rows):
        return _err("itemNotFound", 404, "Row '" + str(row_index) + "' does not exist in the table.")

    body = req["body"]
    if body == None:
        body = {}
    values = _flat_row(body.get("values", None))
    if values == None:
        return _err("invalidRequest", 400, "A row update requires 'values' as a flat array of cell values.")

    rows[row_index] = _fit_row(values)
    doc["rows"] = rows
    tbc = store_collection("tables")
    tbc.update(doc["id"], doc)

    return respond(200, _row_entity(row_index, rows[row_index]))

# on_delete_row removes one row (by 0-based index); the rows below it shift
# up, matching Graph's reindexing behaviour.
# DELETE /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/{index}
# (Bearer) → 204 No Content
def on_delete_row(req):
    err = _require_bearer(req)
    if err != None:
        return err

    doc = _load_table(req)
    if doc == None:
        return _table_not_found(req)

    row_index = _row_index(req)
    rows = doc.get("rows", [])
    if row_index < 0 or row_index >= len(rows):
        return _err("itemNotFound", 404, "Row '" + str(row_index) + "' does not exist in the table.")

    remaining = []
    for i in range(len(rows)):
        if i != row_index:
            remaining.append(rows[i])
    doc["rows"] = remaining
    tbc = store_collection("tables")
    tbc.update(doc["id"], doc)
    return respond(204)

# on_get_range returns the table's range: address, dimensions, and the raw
# values — header row first, then every persisted data row. Adding, updating
# or deleting rows is reflected here immediately (the "stored range").
# GET /v1.0/me/drive/items/{id}/workbook/tables/{name}/range (Bearer)
def on_get_range(req):
    err = _require_bearer(req)
    if err != None:
        return err

    doc = _load_table(req)
    if doc == None:
        return _table_not_found(req)

    columns = doc.get("columns", [])
    rows = doc.get("rows", [])
    values = [_copy_list(columns)]
    for row in rows:
        values.append(_copy_list(row))

    last_col = _column_letter(len(columns))
    last_row = len(rows) + 1
    address = "Sheet1!A1:" + last_col + str(last_row)
    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#workbook/range",
        "address": address,
        "addressLocal": "A1:" + last_col + str(last_row),
        "rowCount": last_row,
        "columnCount": len(columns),
        "values": values,
    })

# --- helpers ---

# _add_rows persists the rows in body.values (2-D array, Graph's shape) at
# body.index (default: the end) and answers with the first added row.
def _add_rows(req, status):
    err = _require_bearer(req)
    if err != None:
        return err

    doc = _load_table(req)
    if doc == None:
        return _table_not_found(req)

    body = req["body"]
    if body == None:
        body = {}
    raw = body.get("values", None)
    if _is_2d_rows(raw) == False or len(raw) == 0:
        return _err("invalidRequest", 400, "Row values must be a non-empty 2-dimensional array.")

    rows = doc.get("rows", [])
    pos = len(rows)
    raw_index = body.get("index", None)
    if raw_index != None:
        index = _int_or_none(raw_index)
        if index == None or index < 0 or index > len(rows):
            return _err("invalidRequest", 400, "'index' must be an integer between 0 and the number of rows.")
        pos = index

    first_index = pos
    for raw_row in raw:
        rows.insert(pos, _fit_row(_copy_list(raw_row)))
        pos = pos + 1
    doc["rows"] = rows
    tbc = store_collection("tables")
    tbc.update(doc["id"], doc)

    return respond(status, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#workbook/tableRow",
        "index": first_index,
        "values": rows[first_index],
    })

# _load_table returns the persisted table doc for the request's drive item
# and table name, seeding the workbook's Table1 on first access, or None
# when the drive item or the table does not exist.
def _load_table(req):
    item_id = req["params"].get("id", "")
    name = req["params"].get("name", "")

    _seed_files()
    fc = store_collection("files")
    if fc.get(item_id) == None:
        return None
    if name != _TABLE_NAME:
        return None

    tbc = store_collection("tables")
    table_id = item_id + "/" + name
    doc = tbc.get(table_id)
    if doc == None:
        seed_rows = []
        for row in _TABLE_SEED_ROWS:
            seed_rows.append(_copy_list(row))
        doc = {
            "id": table_id,
            "item": item_id,
            "name": name,
            "columns": _copy_list(_TABLE_COLUMNS),
            "rows": seed_rows,
        }
        tbc.insert(doc)
    return doc

# _table_not_found is the shared 404 for an unknown drive item or table.
def _table_not_found(req):
    item_id = req["params"].get("id", "")
    name = req["params"].get("name", "")
    _seed_files()
    fc = store_collection("files")
    if fc.get(item_id) == None:
        return _err("itemNotFound", 404, "The resource could not be found.")
    return _err("itemNotFound", 404, "Table '" + name + "' does not exist in the workbook.")

def _table_base_url(req):
    item_id = req["params"].get("id", "")
    name = req["params"].get("name", "")
    return "https://graph.microsoft.com/v1.0/me/drive/items/" + item_id + "/workbook/tables/" + name

def _row_entity(index, row):
    return {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#workbook/tableRow",
        "index": index,
        "values": _copy_list(row),
    }

# _row_index parses the {index} route parameter. Non-numeric or out-of-range
# indexes simply address no row (callers 404), like real Graph.
def _row_index(req):
    raw = req["params"].get("index", "")
    if not _is_digits(raw):
        return -1
    return _to_int(raw)

# _is_2d_rows reports whether v is a non-empty list of lists (Graph's 2-D
# row-values shape). None and scalars are rejected.
def _is_2d_rows(v):
    if type(v) != "list":
        return False
    for row in v:
        if type(row) != "list":
            return False
    return True

# _int_or_none coerces a JSON number to int — the body decoder may surface
# whole numbers as floats — or returns None for non-numeric values.
def _int_or_none(v):
    if type(v) == "int":
        return v
    if type(v) == "float" and v == int(v):
        return int(v)
    return None

# _flat_row normalizes a row-update values body: a flat array is used as-is,
# a 1×N 2-D array is unwrapped, anything else is rejected (None).
def _flat_row(v):
    if type(v) != "list":
        return None
    if len(v) > 0 and type(v[0]) == "list":
        if len(v) != 1:
            return None
        return v[0]
    return v

# _fit_row pads a row with empty cells or truncates it to the table's column
# count so the stored range stays rectangular.
def _fit_row(row):
    width = len(_TABLE_COLUMNS)
    out = _copy_list(row)
    while len(out) < width:
        out.append("")
    return out[:width]

def _copy_list(values):
    out = []
    for v in values:
        out.append(v)
    return out

# _column_letter converts a 1-based column number to its spreadsheet letter
# (1 → A, 26 → Z, 27 → AA).
def _column_letter(n):
    if n < 1:
        return "A"
    letters = ""
    while n > 0:
        rem = (n - 1) % 26
        letters = chr(ord("A") + rem) + letters
        n = (n - 1) // 26
    return letters
