# Calls handler — create a call.
#
# POST /2010-06-01/Accounts/{account_sid}/Calls.json
#   JSON { To, From, Url } -> { sid:"CA...", status:"queued", ... }

# Shared helpers (_require_auth, _next_sid) are preloaded from
# scripts/lib.star.

# on_create_call creates a call record and returns the full call object.
def on_create_call(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_sid = req["params"]["account_sid"]

    body = req["body"]
    if body == None:
        body = {}

    to = body.get("To", "")
    if to == None:
        to = ""
    frm = body.get("From", "")
    if frm == None:
        frm = ""
    url = body.get("Url", "")
    if url == None:
        url = ""

    sid = _next_sid("CA")

    call = {
        "sid": sid,
        "account_sid": account_sid,
        "to": to,
        "from": frm,
        "status": "queued",
        "direction": "outbound-api",
        "api_version": "2010-06-01",
        "price": "-0.01500",
        "price_unit": "USD",
        "duration": "0",
        "uri": "/2010-06-01/Accounts/" + account_sid + "/Calls/" + sid + ".json",
        "date_created": "Mon, 01 Jan 2024 00:00:00 +0000",
        "date_updated": "Mon, 01 Jan 2024 00:00:00 +0000",
        "parent_call_sid": None,
        "phone_number_sid": _next_sid("PN"),
        "forwarded_from": None,
        "caller_name": None,
        "annotation": None,
        "group_sid": None,
        "answered_by": None,
        "start_time": None,
        "end_time": None,
        "start_time_iso": None,
        "end_time_iso": None,
        "subresource_uris": {},
    }

    c = store_collection("calls")
    stored = {}
    for k in call:
        stored[k] = call[k]
    stored["id"] = sid
    c.insert(stored)

    return respond(201, call)
