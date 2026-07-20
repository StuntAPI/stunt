# Import sets handler — ServiceNow Import Set API.
#
# POST /api/now/import/u_my_table
#   body: {"records": [{...}, {...}]}
# -> {result: [{table, sys_id, display_value, status}, ...]}

# Shared helpers from lib.star.

def on_import(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)

    # ServiceNow import set API accepts records as an array or a wrapper.
    records = body.get("records")
    if records == None:
        # If the body itself is a single record, wrap it.
        records = [body]

    results = []
    for rec in records:
        sys_id = _gen_sys_id()
        results.append({
            "table": "u_my_table",
            "sys_id": sys_id,
            "display_value": rec.get("name", rec.get("short_description", "Imported Record")),
            "status": "inserted",
        })

    return respond(201, {
        "result": results,
    })
