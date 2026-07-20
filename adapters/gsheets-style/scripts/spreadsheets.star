# Spreadsheet handlers — create, get, add sheet, batchUpdate.
#
# POST   /v4/spreadsheets                              → create spreadsheet
# GET    /v4/spreadsheets/{spreadsheetId}               → get metadata
# POST   /v4/spreadsheets/{spreadsheetId}/sheets        → add sheet
# POST   /v4/spreadsheets/{ss_id_or_verb}               → dispatch (batchUpdate)
#
# Shared helpers (_bearer, _require_bearer, _parse_range, _seed, etc.) are
# preloaded from scripts/lib.star.

# on_create_spreadsheet creates a new spreadsheet from a request body with
# {properties:{title}, sheets:[...]} and returns the full spreadsheet object.
def on_create_spreadsheet(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    props = body.get("properties", {})
    if props == None:
        props = {}
    title = props.get("title", "Untitled Spreadsheet")
    if title == None:
        title = "Untitled Spreadsheet"

    seq = _seq("ss_seq")
    ss_id = _gen_spreadsheet_id(seq + 1)

    sheets = body.get("sheets", [])
    if sheets == None:
        sheets = []

    sheet_objs = []
    sheet_id_seq = _seq("sheet_id_seq")
    if len(sheets) > 0:
        for i in range(len(sheets)):
            s = sheets[i]
            sprops = s.get("properties", {})
            if sprops == None:
                sprops = {}
            sheet_objs.append({
                "properties": {
                    "sheetId": _gen_sheet_id(sheet_id_seq + i + 1),
                    "title": sprops.get("title", _default_sheet_title(i)),
                    "index": sprops.get("index", i),
                    "gridProperties": {
                        "rowCount": 1000,
                        "columnCount": 26,
                    },
                },
            })
    else:
        sheet_objs.append({
            "properties": {
                "sheetId": _gen_sheet_id(sheet_id_seq + 1),
                "title": "Sheet1",
                "index": 0,
                "gridProperties": {
                    "rowCount": 1000,
                    "columnCount": 26,
                },
            },
        })

    sc = store_collection("spreadsheets")
    sc.insert({
        "spreadsheetId": ss_id,
        "properties": {
            "title": title,
            "locale": "en_US",
            "autoRecalc": "ON_CHANGE",
            "timeZone": "America/Los_Angeles",
            "defaultFormat": {},
        },
        "sheets": sheet_objs,
        "spreadsheetUrl": "https://docs.google.com/spreadsheets/d/" + ss_id + "/edit",
    })

    return respond(200, {
        "spreadsheetId": ss_id,
        "properties": {
            "title": title,
            "locale": "en_US",
            "autoRecalc": "ON_CHANGE",
            "timeZone": "America/Los_Angeles",
            "defaultFormat": {},
        },
        "sheets": sheet_objs,
        "spreadsheetUrl": "https://docs.google.com/spreadsheets/d/" + ss_id + "/edit",
    })

# on_get_spreadsheet returns the full spreadsheet metadata including all
# sheets and their properties.
def on_get_spreadsheet(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    ss_id = req["params"]["spreadsheetId"]
    doc = _find_ss(ss_id)
    if doc == None:
        return _not_found("Spreadsheet not found: " + ss_id)

    # Include any query-param field filtering (fields=...).
    fields = req["query"].get("fields", "")
    if fields != "" and fields != None:
        # Simple support: if fields includes "spreadsheetId", include it.
        # We return the full object anyway (mock simplification).
        pass

    return respond(200, {
        "spreadsheetId": doc["spreadsheetId"],
        "properties": doc["properties"],
        "sheets": doc["sheets"],
        "spreadsheetUrl": doc["spreadsheetUrl"],
    })

# on_add_sheet adds a new sheet (tab) to an existing spreadsheet via the
# POST .../sheets endpoint.
def on_add_sheet(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    ss_id = req["params"]["spreadsheetId"]
    doc = _find_ss(ss_id)
    if doc == None:
        return _not_found("Spreadsheet not found: " + ss_id)

    body = req["body"]
    if body == None:
        body = {}

    # The body may have a top-level "properties" or be the properties itself.
    sprops = body
    if body.get("properties") != None:
        sprops = body["properties"]

    sheets = doc["sheets"]
    new_index = len(sheets)
    sheet_id_seq = _seq("sheet_id_seq")

    sheet_obj = {
        "properties": {
            "sheetId": _gen_sheet_id(sheet_id_seq + new_index + 1),
            "title": sprops.get("title", _default_sheet_title(new_index)),
            "index": sprops.get("index", new_index),
            "gridProperties": {
                "rowCount": 1000,
                "columnCount": 26,
            },
        },
    }

    sheets.append(sheet_obj)
    sc = store_collection("spreadsheets")
    sc.update(doc["id"], doc)

    return respond(200, {
        "spreadsheetId": ss_id,
        "replies": [
            {"addSheet": sheet_obj},
        ],
    })

# on_post_dispatch handles POST to /v4/spreadsheets/{ss_id_or_verb}, which
# may be a spreadsheet-level batchUpdate (the param ends with ":batchUpdate").
def on_post_dispatch(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    param = req["params"]["ss_id_or_verb"]

    if param.endswith(":batchUpdate"):
        ss_id = param[:-len(":batchUpdate")]
        doc = _find_ss(ss_id)
        if doc == None:
            return _not_found("Spreadsheet not found: " + ss_id)
        return _handle_batch_update(req, ss_id, doc)

    # If no verb suffix, treat the body as a create (uncommon path).
    return _bad_request("Unknown operation: " + param)

# _handle_batch_update processes the requests[] array in a spreadsheet
# batchUpdate, modeling addSheet, deleteSheet, updateCells, etc.
def _handle_batch_update(req, ss_id, doc):
    body = req["body"]
    if body == None:
        body = {}
    requests = body.get("requests", [])
    if requests == None:
        requests = []

    replies = []
    sc = store_collection("spreadsheets")

    for i in range(len(requests)):
        r = requests[i]
        if r.get("addSheet") != None:
            sprops = r["addSheet"].get("properties", {})
            if sprops == None:
                sprops = {}
            sheets = doc["sheets"]
            new_index = len(sheets)
            sheet_id_seq = _seq("sheet_id_seq")
            sheet_obj = {
                "properties": {
                    "sheetId": _gen_sheet_id(sheet_id_seq + new_index + 1),
                    "title": sprops.get("title", _default_sheet_title(new_index)),
                    "index": sprops.get("index", new_index),
                    "gridProperties": {
                        "rowCount": 1000,
                        "columnCount": 26,
                    },
                },
            }
            sheets.append(sheet_obj)
            replies.append({"addSheet": sheet_obj})

        elif r.get("deleteSheet") != None:
            sheet_id_to_delete = r["deleteSheet"].get("sheetId", -1)
            sheets = doc["sheets"]
            new_sheets = []
            for s in sheets:
                if s["properties"]["sheetId"] != sheet_id_to_delete:
                    new_sheets.append(s)
            doc["sheets"] = new_sheets
            replies.append({"deleteSheet": {}})

        elif r.get("updateCells") != None:
            # Model updateCells: write cell values into the grid.
            uc = r["updateCells"]
            rows_data = uc.get("rows", [])
            if rows_data == None:
                rows_data = []
            start = uc.get("start", {})
            if start == None:
                start = {}
            grid_range = start.get("gridRange", {})
            if grid_range == None:
                grid_range = {}
            sheet_title = _sheet_title_for_id(doc, grid_range.get("sheetId", 0))
            start_row = grid_range.get("startRowIndex", 0)
            start_col = grid_range.get("startColumnIndex", 0)
            for ri in range(len(rows_data)):
                row_data = rows_data[ri]
                values_list = row_data.get("values", [])
                if values_list == None:
                    values_list = []
                for ci in range(len(values_list)):
                    cell_data = values_list[ci]
                    uv = cell_data.get("userEnteredValue", {})
                    if uv == None:
                        uv = {}
                    val = ""
                    if uv.get("stringValue") != None:
                        val = uv["stringValue"]
                    elif uv.get("numberValue") != None:
                        val = str(uv["numberValue"])
                    elif uv.get("boolValue") != None:
                        val = str(uv["boolValue"])
                    _set_cell(ss_id, sheet_title, start_row + ri, start_col + ci, val)
            replies.append({"updateCells": {}})

        elif r.get("autoResizeDimensions") != None:
            replies.append({"autoResizeDimensions": {}})

        elif r.get("repeatCell") != None:
            replies.append({"repeatCell": {}})

        else:
            # Unknown request type — echo it back as a no-op reply.
            replies.append({})

    sc.update(doc["id"], doc)

    return respond(200, {
        "spreadsheetId": ss_id,
        "replies": replies,
        "updatedSpreadsheet": doc,
    })

# _sheet_title_for_id finds the sheet title for a given sheetId within a doc.
def _sheet_title_for_id(doc, sheet_id):
    for s in doc["sheets"]:
        if s["properties"]["sheetId"] == sheet_id:
            return s["properties"]["title"]
    return "Sheet1"
