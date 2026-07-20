# Values handlers — the heart of the A1-notation grid model.
#
# GET    /v4/spreadsheets/{spreadsheetId}/values/{range}           → read cells
# PUT    /v4/spreadsheets/{spreadsheetId}/values/{range}           → write cells
# POST   /v4/spreadsheets/{spreadsheetId}/values/{range}:append    → append rows
# POST   /v4/spreadsheets/{spreadsheetId}/values/{range}:clear     → clear cells
# POST   /v4/spreadsheets/{spreadsheetId}/values:batchGet          → multi-range read
# POST   /v4/spreadsheets/{spreadsheetId}/values:batchUpdate       → multi-range write
#
# STATEFUL: cells written via PUT are readable by a subsequent GET. The grid
# model is keyed by (spreadsheetId, sheetTitle, row, col) in KV storage.
#
# Shared helpers (_bearer, _require_bearer, _parse_range, _set_cell,
# _get_cell, _build_range_str, etc.) are preloaded from scripts/lib.star.

# on_get_values reads cells from the grid and returns them as a 2D array
# in Google's {range, majorDimension, values} format.
def on_get_values(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    ss_id = req["params"]["spreadsheetId"]
    range_str = req["params"]["range"]

    doc = _find_ss(ss_id)
    if doc == None:
        return _not_found("Spreadsheet not found: " + ss_id)

    parsed = _parse_range(range_str)
    if parsed == None:
        return _bad_request("Unable to parse range: " + range_str)

    sheet = parsed["sheet"]
    if sheet == "":
        sheet = "Sheet1"

    values = _read_grid(ss_id, sheet, parsed["row1"], parsed["col1"], parsed["row2"], parsed["col2"])

    out_range = _build_range_str(sheet, parsed["row1"], parsed["col1"],
                                 parsed["row1"] + len(values) - 1,
                                 parsed["col1"] + _max_row_width(values) - 1)

    return respond(200, {
        "range": out_range,
        "majorDimension": "ROWS",
        "values": values,
    })

# on_update_values (PUT) writes cells into the grid from a 2D values array.
def on_update_values(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    ss_id = req["params"]["spreadsheetId"]
    range_str = req["params"]["range"]

    doc = _find_ss(ss_id)
    if doc == None:
        return _not_found("Spreadsheet not found: " + ss_id)

    body = req["body"]
    if body == None:
        body = {}
    values = body.get("values", [])
    if values == None:
        values = []

    parsed = _parse_range(range_str)
    if parsed == None:
        return _bad_request("Unable to parse range: " + range_str)

    sheet = parsed["sheet"]
    if sheet == "":
        sheet = "Sheet1"

    # Write each cell.
    num_rows = len(values)
    num_cols = _max_row_width(values)
    for ri in range(len(values)):
        row = values[ri]
        for ci in range(len(row)):
            val = row[ci]
            if val == None:
                val = ""
            _set_cell(ss_id, sheet, parsed["row1"] + ri, parsed["col1"] + ci, str(val))

    out_range = _build_range_str(sheet, parsed["row1"], parsed["col1"],
                                 parsed["row1"] + num_rows - 1,
                                 parsed["col1"] + max(num_cols - 1, 0))

    return respond(200, {
        "spreadsheetId": ss_id,
        "updatedRange": out_range,
        "updatedRows": num_rows,
        "updatedColumns": num_cols,
        "updatedCells": _count_cells(values),
    })

# on_values_post dispatches POST to values/{range_verb} — either :append
# or :clear.
def on_values_post(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    range_verb = req["params"]["range_verb"]

    # Determine the verb suffix (:append or :clear).
    if range_verb.endswith(":append"):
        actual_range = range_verb[:-len(":append")]
        return _append_values(req, actual_range)
    elif range_verb.endswith(":clear"):
        actual_range = range_verb[:-len(":clear")]
        return _clear_values(req, actual_range)

    return _bad_request("Unknown values operation: " + range_verb)

# _append_values appends rows after the last row of data in the given range.
def _append_values(req, range_str):
    ss_id = req["params"]["spreadsheetId"]
    doc = _find_ss(ss_id)
    if doc == None:
        return _not_found("Spreadsheet not found: " + ss_id)

    body = req["body"]
    if body == None:
        body = {}
    values = body.get("values", [])
    if values == None:
        values = []

    parsed = _parse_range(range_str)
    if parsed == None:
        return _bad_request("Unable to parse range: " + range_str)

    sheet = parsed["sheet"]
    if sheet == "":
        sheet = "Sheet1"

    # Find the last populated row in the sheet within the column range.
    last_row = -1
    r = parsed["row1"]
    # Scan from row1 downward to find the last row with any data.
    while r <= parsed["row2"]:
        row_has_data = False
        for c in range(parsed["col1"], parsed["col2"] + 1):
            if _get_cell(ss_id, sheet, r, c) != "":
                row_has_data = True
        if row_has_data:
            last_row = r
        r = r + 1

    start_row = last_row + 1
    num_cols = _max_row_width(values)

    # Write the appended rows.
    for ri in range(len(values)):
        row = values[ri]
        for ci in range(len(row)):
            val = row[ci]
            if val == None:
                val = ""
            _set_cell(ss_id, sheet, start_row + ri, parsed["col1"] + ci, str(val))

    updated_range = _build_range_str(sheet, start_row, parsed["col1"],
                                     start_row + len(values) - 1,
                                     parsed["col1"] + max(num_cols - 1, 0))

    table_range = ""
    if last_row >= 0:
        table_range = _build_range_str(sheet, parsed["row1"], parsed["col1"],
                                       last_row, parsed["col2"])

    return respond(200, {
        "spreadsheetId": ss_id,
        "tableRange": table_range,
        "updates": {
            "spreadsheetId": ss_id,
            "updatedRange": updated_range,
            "updatedRows": len(values),
            "updatedColumns": num_cols,
            "updatedCells": _count_cells(values),
        },
    })

# _clear_values clears all cells in the given range.
def _clear_values(req, range_str):
    ss_id = req["params"]["spreadsheetId"]
    doc = _find_ss(ss_id)
    if doc == None:
        return _not_found("Spreadsheet not found: " + ss_id)

    parsed = _parse_range(range_str)
    if parsed == None:
        return _bad_request("Unable to parse range: " + range_str)

    sheet = parsed["sheet"]
    if sheet == "":
        sheet = "Sheet1"

    cleared = 0
    for r in range(parsed["row1"], parsed["row2"] + 1):
        for c in range(parsed["col1"], parsed["col2"] + 1):
            if _get_cell(ss_id, sheet, r, c) != "":
                cleared = cleared + 1
            _set_cell(ss_id, sheet, r, c, "")

    return respond(200, {
        "spreadsheetId": ss_id,
        "clearedRange": _build_range_str(sheet, parsed["row1"], parsed["col1"],
                                         parsed["row2"], parsed["col2"]),
        "clearedCells": cleared,
    })

# on_batch_get reads multiple ranges in a single request.
def on_batch_get(req):
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
    ranges = body.get("ranges", [])
    if ranges == None:
        ranges = []

    value_ranges = []
    for r in ranges:
        parsed = _parse_range(r)
        if parsed == None:
            continue
        sheet = parsed["sheet"]
        if sheet == "":
            sheet = "Sheet1"
        values = _read_grid(ss_id, sheet, parsed["row1"], parsed["col1"],
                            parsed["row2"], parsed["col2"])
        value_ranges.append({
            "range": _build_range_str(sheet, parsed["row1"], parsed["col1"],
                                      parsed["row1"] + len(values) - 1,
                                      parsed["col1"] + _max_row_width(values) - 1),
            "majorDimension": "ROWS",
            "values": values,
        })

    return respond(200, {
        "spreadsheetId": ss_id,
        "valueRanges": value_ranges,
    })

# on_batch_update writes multiple ranges in a single request.
def on_batch_update(req):
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
    data = body.get("data", [])
    if data == None:
        data = []

    responses = []
    total_cells = 0
    total_rows = 0
    total_cols = 0

    for d in data:
        range_str = d.get("range", "")
        if range_str == None:
            range_str = ""
        values = d.get("values", [])
        if values == None:
            values = []

        parsed = _parse_range(range_str)
        if parsed == None:
            continue

        sheet = parsed["sheet"]
        if sheet == "":
            sheet = "Sheet1"

        num_rows = len(values)
        num_cols = _max_row_width(values)
        for ri in range(len(values)):
            row = values[ri]
            for ci in range(len(row)):
                val = row[ci]
                if val == None:
                    val = ""
                _set_cell(ss_id, sheet, parsed["row1"] + ri, parsed["col1"] + ci, str(val))

        cells = _count_cells(values)
        total_cells = total_cells + cells
        total_rows = total_rows + num_rows
        total_cols = total_cols + num_cols

        responses.append({
            "spreadsheetId": ss_id,
            "updatedRange": _build_range_str(sheet, parsed["row1"], parsed["col1"],
                                             parsed["row1"] + num_rows - 1,
                                             parsed["col1"] + max(num_cols - 1, 0)),
            "updatedRows": num_rows,
            "updatedColumns": num_cols,
            "updatedCells": cells,
        })

    return respond(200, {
        "spreadsheetId": ss_id,
        "totalUpdatedRows": total_rows,
        "totalUpdatedColumns": total_cols,
        "totalUpdatedCells": total_cells,
        "responses": responses,
    })

# === Grid helpers ===

# _read_grid reads cells from the grid into a 2D values array, trimming
# trailing empty rows and columns (matching Google's behaviour).
def _read_grid(ss_id, sheet, row1, col1, row2, col2):
    if row2 > 999:
        row2 = 999
    if col2 > 99:
        col2 = 99

    result = []
    max_col_used = -1
    for r in range(row1, row2 + 1):
        row_vals = []
        for c in range(col1, col2 + 1):
            row_vals.append(_get_cell(ss_id, sheet, r, c))
        result.append(row_vals)

    # Trim trailing empty rows.
    while len(result) > 0 and _row_all_empty(result[len(result) - 1]):
        result = result[:len(result) - 1]

    # Trim trailing empty columns across all rows.
    if len(result) > 0:
        width = len(result[0])
        max_col_used = 0
        for r in range(len(result)):
            for c in range(width - 1, -1, -1):
                if result[r][c] != "":
                    if c + 1 > max_col_used:
                        max_col_used = c + 1
                    break
        if max_col_used < width:
            for r in range(len(result)):
                result[r] = result[r][:max_col_used]

    return result

# _row_all_empty reports whether every value in a row is "".
def _row_all_empty(row):
    for v in row:
        if v != "":
            return False
    return True

# _max_row_width returns the width of the widest row in a 2D values array.
def _max_row_width(values):
    max_w = 0
    for row in values:
        if len(row) > max_w:
            max_w = len(row)
    return max_w

# _count_cells counts non-None cells in a 2D values array.
def _count_cells(values):
    count = 0
    for row in values:
        for v in row:
            if v != None:
                count = count + 1
    return count
