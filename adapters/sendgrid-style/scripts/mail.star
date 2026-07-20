# Mail handlers — stateful send and list.
#
# POST /v3/mail/send   (Bearer; JSON {personalizations, from, content, ...})
#   -> 202 Accepted (empty body — exactly like real SendGrid)
# GET  /v3/messages?limit=N
#   -> {messages: [...]}  (debug/retrieval endpoint for asserting sent mail)
#
# Mail records are STATEFUL: a message sent via POST appears in the GET
# messages endpoint, enabling round-trip testing locally.

# Shared helpers (_bearer, _require_auth, _next_msg_id, _extract_emails)
# are preloaded from scripts/lib.star.

# on_send_mail creates a mail record and returns 202 Accepted (empty body).
def on_send_mail(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    # Extract personalizations (recipients)
    personalizations = body.get("personalizations", [])
    if personalizations == None:
        personalizations = []

    # Extract "from" address
    from_obj = body.get("from", {})
    if from_obj == None:
        from_obj = {}

    # Extract content
    content_list = body.get("content", [])
    if content_list == None:
        content_list = []

    # Extract subject (may be in personalizations or top-level)
    subject = ""
    for p in personalizations:
        p_subject = p.get("subject", None)
        if p_subject != None and p_subject != "":
            subject = p_subject
            break
    if subject == "":
        subject = body.get("subject", "")
        if subject == None:
            subject = ""

    # Extract recipients from personalizations
    recipients = _extract_emails(personalizations)

    msg_id = _next_msg_id()

    doc = {
        "id": msg_id,
        "from": from_obj,
        "to": recipients,
        "subject": subject,
        "content": content_list,
        "template_id": body.get("template_id", None),
        "categories": body.get("categories", []),
        "custom_args": body.get("custom_args", {}),
        "headers": body.get("headers", {}),
        "created_at": _now_iso(),
        "status": "delivered",
    }

    c = store_collection("mail")
    c.insert(doc)

    # Emit a webhook event (fire-and-forget).
    events_emit("mail.sent", {
        "message_id": msg_id,
        "email": recipients,
        "event": "processed",
        "sg_event_id": "event_" + msg_id,
        "sg_message_id": msg_id,
        "timestamp": 1705312800,
    })
    events_emit("mail.delivered", {
        "message_id": msg_id,
        "email": recipients,
        "event": "delivered",
        "sg_event_id": "event_" + msg_id,
        "sg_message_id": msg_id,
        "timestamp": 1705312801,
    })

    # Real SendGrid returns 202 Accepted with empty body.
    return respond(202, "", {
        "X-Message-Id": msg_id,
        "Access-Control-Allow-Origin": "https://sendgrid.com",
    })

# on_list_messages returns sent mail for assertion/testing.
def on_list_messages(req):
    err = _require_auth(req)
    if err != None:
        return err

    query = req.get("query")
    if query == None:
        query = {}
    limit_str = query.get("limit", "50")
    if limit_str == None:
        limit_str = "50"
    limit = _to_int(limit_str)
    if limit == 0:
        limit = 50

    c = store_collection("mail")
    all_mail = c.list()

    result = []
    for m in all_mail:
        result.append({
            "id": m.get("id", ""),
            "from": m.get("from", {}),
            "to": m.get("to", []),
            "subject": m.get("subject", ""),
            "content": m.get("content", []),
            "created_at": m.get("created_at", ""),
            "status": m.get("status", ""),
        })

    if len(result) > limit:
        result = result[:limit]

    return respond(200, {"messages": result})
