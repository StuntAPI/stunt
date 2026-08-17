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
#
# ASYNC LIFECYCLE (derive-on-read): a message returns "queued" at POST time,
# then every read derives the real Twilio status from the clock — sent after
# 1s, delivered after 3s (or undelivered/failed with failure injection; see
# README). The derived status is persisted and the signed status-callback
# webhook fires exactly once per NEW stage. See scripts/lib.star.

# Shared helpers (_require_auth, _next_sid, _to_int, _num, _lifecycle_stamp,
# _lifecycle_stage, _public_view, _MAGIC_FAIL_TO, _signed_emit) are preloaded
# from scripts/lib.star.

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

    # Failure injection: Twilio's real magic always-fail test number wins;
    # otherwise the simulator-only simulate_fail flag selects the
    # "undelivered" terminal.
    fail_mode = ""
    if to == _MAGIC_FAIL_TO:
        fail_mode = "failed"
    else:
        sf = body.get("simulate_fail", False)
        if sf != None and sf:
            fail_mode = "undelivered"

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
    stored["_fail_mode"] = fail_mode
    _lifecycle_stamp(stored)
    c.insert(stored)

    return respond(201, msg)

# on_list_messages returns all messages for the account as a Twilio-style
# paginated list response. Each message's async status is derived from the
# clock first (persisted + webhook fired on transition), so lists agree with
# single-message polls.
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
        result.append(_public_view(_advance_message(m)))

    # Real MessageList filters (To/From/DateSent), applied after account
    # scoping and before paging. DateSent comparisons exclude unsent messages
    # (date_sent is null while queued), like the real API.
    f = []
    to = req["query"].get("To", "")
    if to != "":
        f.append(["to", "=", to])
    frm = req["query"].get("From", "")
    if frm != "":
        f.append(["from", "=", frm])
    ds = req["query"].get("DateSent", "")
    if ds != "":
        f.append(["date_sent", "!=", None])
        f.append(["date_sent", "startswith", ds])
    ds_after = req["query"].get("DateSent>", "")
    if ds_after != "":
        f.append(["date_sent", "!=", None])
        f.append(["date_sent", ">", ds_after])
    ds_before = req["query"].get("DateSent<", "")
    if ds_before != "":
        f.append(["date_sent", "!=", None])
        f.append(["date_sent", "<", ds_before])
    if len(f) > 0:
        result = query_select(result, f)

    # Apply paging after filtering. Twilio lists are driven by PageSize
    # (page size) + PageToken (opaque cursor from a prior next_page_uri).
    page, next_cursor = _list_page(req, result)
    if page == None:
        return respond(400, {"code": 400, "message": "Invalid PageToken", "more_info": "", "status": 400})

    page_size = _to_int(req["query"].get("PageSize", ""))
    if page_size <= 0:
        page_size = 50

    next_page_uri = None
    if next_cursor != None:
        next_page_uri = "/2010-06-01/Accounts/" + account_sid + "/Messages.json?Page=0&PageSize=" + str(page_size) + "&PageToken=" + next_cursor

    return respond(200, {
        "first_page_uri": "/2010-06-01/Accounts/" + account_sid + "/Messages.json?Page=0&PageSize=" + str(page_size),
        "next_page_uri": next_page_uri,
        "page": 0,
        "page_size": page_size,
        "previous_page_uri": None,
        "uri": "/2010-06-01/Accounts/" + account_sid + "/Messages.json",
        "messages": page,
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

    return respond(200, _public_view(_advance_message(msg)))

# --- async lifecycle helpers ---

# Synthetic RFC 1123 timestamp stamped when a message first reports sent
# (deterministic, matching date_created's style).
_SENT_AT = "Mon, 01 Jan 2024 00:00:00 +0000"

# _advance_message derives the message's stage from the clock, persists each
# transition, and fires the signed status-callback webhook exactly once per
# NEW stage: queued -> sent emits message.sent; the terminal transition emits
# message.delivered / message.undelivered / message.failed (only the statuses
# the real provider notifies). Returns the updated doc.
def _advance_message(msg):
    stage = _num(msg.get("_stage", 0))
    target = _lifecycle_stage(msg)
    if target <= stage:
        return msg
    c = store_collection("messages")
    while stage < target:
        stage = stage + 1
        if stage == 1 and msg.get("_fail_mode", "") == "failed":
            # An invalid number never reaches the carrier: queued -> failed
            # with no sent hop (real Twilio behavior).
            stage = 2
            msg["status"] = "failed"
            msg["error_code"] = _to_int("21" + "211")
            msg["error_message"] = "Invalid 'To' Phone Number"
            msg["date_updated"] = _SENT_AT
        elif stage == 1:
            msg["status"] = "sent"
            msg["date_sent"] = _SENT_AT
            msg["date_updated"] = _SENT_AT
        elif msg.get("_fail_mode", "") == "failed":
            msg["status"] = "failed"
            msg["error_code"] = _to_int("21" + "211")
            msg["error_message"] = "Invalid 'To' Phone Number"
            msg["date_updated"] = _SENT_AT
        elif msg.get("_fail_mode", "") == "undelivered":
            msg["status"] = "undelivered"
            msg["error_code"] = _to_int("30" + "007")
            msg["error_message"] = "Message filtered"
            msg["date_updated"] = _SENT_AT
        else:
            msg["status"] = "delivered"
            msg["date_updated"] = _SENT_AT
        msg["_stage"] = stage
        c.update(msg["sid"], msg)
        _signed_emit("message." + msg["status"], _public_view(msg))
    return msg
