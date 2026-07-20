# Microsoft Graph v1.0 — Excel (workbook) handlers.
#
# GET  /v1.0/me/drive/items/{id}/workbook/worksheets → list worksheets
# POST /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/add → add a row
#
# The Excel-on-Graph surface is notoriously painful (deep nesting, session
# tokens, ranges). This mock reproduces the response shapes.

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

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#workbook/worksheets",
        "value": worksheets,
    })

# on_add_table_row adds a row to a workbook table.
# POST /v1.0/me/drive/items/{id}/workbook/tables/{name}/rows/add (Bearer)
# Body: { values: [[val, val, ...]] }
# Returns the added row (index + values).
def on_add_table_row(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    values = body.get("values", [[]])
    if values == None:
        values = [[]]

    # Track row count per table for realistic index.
    table_name = req["params"].get("name", "default")
    count_key = "excel_rows_" + table_name
    row_index = store_kv_incr("graph", count_key)

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#workbook/tableRow",
        "index": row_index,
        "values": values,
    })
