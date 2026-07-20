# Messages handlers — stateful send, list, and retrieve.
#
# POST /2010-06-01/Accounts/{account_sid}/Messages.json
#   JSON { To, From, Body } -> { sid:"SM...", body, status:"queued", ... }
# GET  /2010-06-01/Accounts/{account_sid}/Messages.json
#   -> { first_page_uri, next_page_uri, messages: [...] }
# GET  /2010-06-01/Accounts/{account_sid}/Messages/{sid}.json
#   -> { sid, body, status, ... }
#
# Messages are STATEFUL: a message POSTed appears in the GET list.

# Shared helpers (_require_auth, _next_sid, _to_int) are preloaded from
# scripts/lib.star.

# on_send_message creates a message and returns the full message object.
def on_send_message(req):
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
    msg_body = body.get("Body", "")
    if msg_body == None:
        msg_body = ""

    sid = _next_sid("SM")

    msg = {
        "sid": sid,
        "account_sid": account_sid,
        "to": to,
        "from": frm,
        "body": msg_body,
        "status": "queued",
        "direction": "outbound-api",
        "api_version": "2010-06-01",
        "price": "-0.00750",
        "price_unit": "USD",
        "uri": "/2010-06-01/Accounts/" + account_sid + "/Messages/" + sid + ".json",
        "date_created": "Mon, 01 Jan 2024 00:00:00 +0000",
        "date_sent": None,
        "date_updated": "Mon, 01 Jan 2024 00:00:00 +0000",
        "error_code": None,
        "error_message": None,
        "num_segments": "1",
        "num_media": "0",
        "messaging_service_sid": None,
        "subresource_uris": {},
    }

    c = store_collection("messages")
    stored = {}
    for k in msg:
        stored[k] = msg[k]
    stored["id"] = sid
    c.insert(stored)

    # Emit webhook event (fire-and-forget).
    events_emit("message.sent", msg)

    return respond(201, msg)

# on_list_messages returns all messages for the account as a Twilio-style
# paginated list response.
def on_list_messages(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_sid = req["params"]["account_sid"]

    c = store_collection("messages")
    all_msgs = c.list()
    result = []
    for m in all_msgs:
        if m.get("account_sid", "") != account_sid:
            continue
        result.append(m)

    return respond(200, {
        "first_page_uri": "/2010-06-01/Accounts/" + account_sid + "/Messages.json?Page=0&PageSize=50",
        "next_page_uri": None,
        "page": 0,
        "page_size": 50,
        "previous_page_uri": None,
        "uri": "/2010-06-01/Accounts/" + account_sid + "/Messages.json",
        "messages": result,
    })

# on_get_message retrieves a single message by SID.
def on_get_message(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_sid = req["params"]["account_sid"]
    sid = req["params"]["sid"]
    # Strip optional .json suffix (Twilio uses .json in URLs but the route
    # matcher treats {sid}.json as a literal, so we accept bare {sid}).
    if len(sid) > 5 and sid[len(sid) - 5:] == ".json":
        sid = sid[:len(sid) - 5]

    c = store_collection("messages")
    msg = c.get(sid)
    if msg == None:
        return respond(404, {
            "code": 20404,
            "message": "The requested resource was not found",
            "more_info": "https://www.twilio.com/docs/errors/20404",
            "status": 404,
        })

    return respond(200, msg)
