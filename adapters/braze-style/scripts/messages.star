# Message handlers — Braze REST API.
#
# POST /messages/send        → send message (validates message/campaign ids)
# POST /messages/schedule/create → schedule a message send
# GET  /messages/scheduled   → list upcoming scheduled broadcasts
#
# ASYNC LIFECYCLE (derive-on-read): a scheduled message is stored with the
# unix time it should send at; every read of GET /messages/scheduled derives
# the real state from the clock — still "scheduled" before the time, "sent"
# once it passes — persists the transition (schedule doc + dispatch record
# BEFORE the webhook is emitted), and only still-upcoming broadcasts remain
# in the list, like the real "upcoming scheduled campaigns and Canvases"
# endpoint.
#
# Shared helpers (_require_auth, _body_of, _bad_body, _fatal, _campaign,
# _next_dispatch_id, _next_schedule_id, _parse_iso8601, _record_dispatch,
# _emit_if_subscribed, ...) are preloaded from scripts/lib.star.

_MAX_REQUEST_IDS = 50 # fatal error threshold for external ids in one request

# _channels lists the messaging objects a request carries (apple_push, email,
# sms, ...).
def _channels(messages):
    out = []
    for ch in messages:
        out.append(ch)
    return out

# _variation_state checks message_variation_id usage against the campaign's
# variations. Returns (None) when fine, else a fatal error response carrying
# the documented "Message Variant Unspecified" / "Invalid Message Variant".
def _variation_state(campaign, messages):
    variations = campaign.get("message_variation_ids", [])
    if variations == None:
        variations = []
    found = []
    for ch in messages:
        obj = messages[ch]
        if obj == None or type(obj) != "dict":
            continue
        mv = obj.get("message_variation_id", None)
        if mv != None and mv != "":
            found.append(mv)
    if len(found) == 0:
        return _fatal("Message Variant Unspecified",
            "You provide a campaign ID but no message variation ID.")
    for mv in found:
        if mv not in variations:
            return _fatal("Invalid Message Variant",
                "The message variation ID '" + str(mv) + "' doesn't match any of that campaign's messages.")
    return None

# _recipients_ok applies the shared audience rules: at least one of
# external_user_ids / segment_id / audience, broadcast set correctly, and at
# most 50 external ids. Returns a fatal error response or None.
def _recipients_ok(body):
    external_ids = body.get("external_user_ids", [])
    if external_ids == None:
        external_ids = []
    broadcast = body.get("broadcast", False)
    if broadcast == None:
        broadcast = False
    segment_id = body.get("segment_id", None)
    audience = body.get("audience", None)
    if broadcast:
        if len(external_ids) > 0:
            return _fatal("Bad Request",
                "When broadcast is true a recipients list cannot be included.")
    if len(external_ids) > _MAX_REQUEST_IDS:
        return _fatal("The max number of external_ids and aliases per request was exceeded",
            "Caused by calling more than " + str(_MAX_REQUEST_IDS) + " external ids.")
    if len(external_ids) == 0 and not broadcast and segment_id == None and audience == None:
        return _fatal("No Recipients",
            "There are no external IDs or segment IDs or no push tokens in the request.")
    return None

def on_send(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body, ok = _body_of(req)
    if not ok:
        return _bad_body()

    external_ids = body.get("external_user_ids", [])
    if external_ids == None:
        external_ids = []
    messages = body.get("messages", None)
    if messages == None or type(messages) != "dict" or len(messages) == 0:
        return _fatal("No message to send",
            "No payload is specified for the message.")

    # Campaign ids (optional here) are validated against the campaigns store.
    campaign_id = body.get("campaign_id", None)
    if campaign_id == None or campaign_id == "":
        campaign_id = None
    else:
        campaign = _campaign(campaign_id)
        if campaign == None:
            return _fatal("Invalid Campaign ID",
                "No Messaging API campaign was found for the campaign ID you provided.")
        err = _variation_state(campaign, messages)
        if err != None:
            return err

    err = _recipients_ok(body)
    if err != None:
        return err

    dispatch_id = _next_dispatch_id()

    # Persist the dispatch record BEFORE emitting the webhook so the emitted
    # event always reflects stored state.
    _record_dispatch(dispatch_id, campaign_id, None, _channels(messages), len(external_ids), "sent")

    # Deliver an unsigned webhook event for the dispatch (unsigned-by-design:
    # Braze's webhook channel applies no provider signature — see lib.star).
    _emit_if_subscribed("message.sent", {
        "dispatch_id": dispatch_id,
        "campaign_id": campaign_id,
        "recipients": len(external_ids),
        "channels": _channels(messages),
        "timestamp": clock.now_rfc3339(),
    })

    out = {
        "message": "success",
        "dispatch_id": dispatch_id,
    }
    send_id = body.get("send_id", None)
    if send_id != None and send_id != "":
        out["send_id"] = send_id
    return respond(200, out)

def on_schedule_create(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body, ok = _body_of(req)
    if not ok:
        return _bad_body()

    schedule = body.get("schedule", None)
    if schedule == None or type(schedule) != "dict":
        return _fatal("Bad Request",
            "You must include a schedule object with a 'time'.")
    time_s = schedule.get("time", None)
    send_at = _parse_iso8601(time_s)
    if send_at == None:
        return _fatal("Bad Request",
            "Cannot parse send_at datetime.")

    campaign_id = body.get("campaign_id", None)
    if campaign_id == None or campaign_id == "":
        campaign_id = None
    else:
        campaign = _campaign(campaign_id)
        if campaign == None:
            return _fatal("Invalid Campaign ID",
                "No Messaging API campaign was found for the campaign ID you provided.")

    messages = body.get("messages", None)
    if messages == None:
        messages = {}
    if type(messages) != "dict":
        messages = {}
    if campaign_id == None and len(messages) == 0:
        return _fatal("No message to send",
            "No payload is specified for the message.")

    err = _recipients_ok(body)
    if err != None:
        return err

    external_ids = body.get("external_user_ids", [])
    if external_ids == None:
        external_ids = []
    broadcast = body.get("broadcast", False)
    if broadcast == None:
        broadcast = False
    in_local = schedule.get("in_local_time", False)
    if in_local == None:
        in_local = False
    at_optimal = schedule.get("at_optimal_time", False)
    if at_optimal == None:
        at_optimal = False

    dispatch_id = _next_dispatch_id()
    schedule_id = _next_schedule_id()

    sc = store_collection("schedules")
    sc.insert({
        "id": schedule_id,
        "name": "API Schedule " + schedule_id[:8],
        "dispatch_id": dispatch_id,
        "campaign_id": campaign_id,
        "broadcast": broadcast,
        "external_user_ids": external_ids,
        "schedule": {
            "time": time_s,
            "in_local_time": in_local,
            "at_optimal_time": at_optimal,
        },
        "_send_at": send_at,
        "_status": "scheduled",
        "_sent_at": None,
        "created_at": clock.now_rfc3339(),
        "updated_at": clock.now_rfc3339(),
    })

    return respond(200, {
        "message": "success",
        "dispatch_id": dispatch_id,
        "schedule_id": schedule_id,
    })

# _advance_schedule derives a schedule's state from the clock and persists
# each new stage: when now passes the scheduled time the schedule becomes
# "sent", a dispatch record is written, and the message.sent webhook fires
# exactly once per schedule (persist before emit). Returns the updated doc.
def _advance_schedule(doc):
    if doc.get("_status", "scheduled") != "scheduled":
        return doc
    if clock.now_unix() < _num(doc.get("_send_at", 0)):
        return doc
    doc["_status"] = "sent"
    doc["_sent_at"] = clock.now_rfc3339()
    dispatch_id = doc.get("dispatch_id", None)
    if dispatch_id == None or dispatch_id == "":
        dispatch_id = _next_dispatch_id()
        doc["dispatch_id"] = dispatch_id
    sc = store_collection("schedules")
    sc.update(doc["id"], doc)
    channels = []
    if doc.get("campaign_id", None) != None:
        campaign = _campaign(doc.get("campaign_id"))
        if campaign != None:
            ch = campaign.get("channels", [])
            if ch != None:
                channels = ch
    recipients = len(doc.get("external_user_ids", []))
    _record_dispatch(dispatch_id, doc.get("campaign_id", None), doc.get("id"), channels, recipients, "sent")
    _emit_if_subscribed("message.sent", {
        "dispatch_id": dispatch_id,
        "campaign_id": doc.get("campaign_id", None),
        "schedule_id": doc.get("id"),
        "recipients": recipients,
        "channels": channels,
        "timestamp": doc.get("_sent_at", ""),
    })
    return doc

def on_scheduled(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    query = req.get("query", {})
    if query == None:
        query = {}
    end_time = query.get("end_time", "")
    end_at = _parse_iso8601(end_time)
    if end_time == None or end_time == "" or end_at == None:
        return _fatal("Bad Request",
            "Cannot parse end_time datetime. end_time is required and must be ISO 8601.")

    sc = store_collection("schedules")
    for doc in sc.list():
        _advance_schedule(doc)

    upcoming = query_select(sc.list(), [
        ["_status", "=", "scheduled"],
        ["_send_at", "<=", end_at],
    ])
    broadcasts = []
    for doc in upcoming:
        schedule = doc.get("schedule", {})
        if schedule == None:
            schedule = {}
        schedule_type = "UTC"
        if schedule.get("at_optimal_time", False):
            schedule_type = "intelligent_delivery"
        elif schedule.get("in_local_time", False):
            schedule_type = "local_time_zones"
        broadcasts.append({
            "name": doc.get("name", ""),
            "id": doc.get("campaign_id", None),
            "type": "Campaign",
            "tags": [],
            "next_send_time": schedule.get("time", None),
            "schedule_type": schedule_type,
        })

    return respond(200, {
        "scheduled_broadcasts": broadcasts,
    })
