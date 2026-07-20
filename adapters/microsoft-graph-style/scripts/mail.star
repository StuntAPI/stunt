# Microsoft Graph v1.0 — Outlook mail handlers.
#
# GET  /v1.0/me/mailFolders       → mail folders
# GET  /v1.0/me/messages          → list messages (OData)
# GET  /v1.0/me/messages/{id}     → get a single message
# POST /v1.0/me/sendMail          → send a message (202, STATEFUL)
#
# Messages are STATEFUL: a message sent via POST /sendMail appears in the
# GET /me/messages list (in the Sent Items folder), enabling send/list
# round-trip testing.

# on_list_folders returns the default mail folders.
# GET /v1.0/me/mailFolders (Bearer)
def on_list_folders(req):
    err = _require_bearer(req)
    if err != None:
        return err

    folders = [
        {"id": "inbox", "displayName": "Inbox", "totalItemCount": 3, "unreadItemCount": 1},
        {"id": "sentitems", "displayName": "Sent Items", "totalItemCount": 0, "unreadItemCount": 0},
        {"id": "drafts", "displayName": "Drafts", "totalItemCount": 0, "unreadItemCount": 0},
        {"id": "junkemail", "displayName": "Junk Email", "totalItemCount": 0, "unreadItemCount": 0},
    ]

    # Count actual messages in each folder.
    mc = store_collection("messages")
    all_msgs = mc.list()
    for f in folders:
        count = 0
        for m in all_msgs:
            if m.get("folder", "") == f["id"]:
                count = count + 1
        if count > 0:
            f["totalItemCount"] = count

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users('me')/mailFolders",
        "value": folders,
    })

# on_list_messages returns messages for the current user.
# GET /v1.0/me/messages (Bearer)
def on_list_messages(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_inbox()
    mc = store_collection("messages")
    docs = mc.list()
    entities = []
    for m in docs:
        entities.append(_message_entity(m))

    base_url = "https://graph.microsoft.com/v1.0/me/messages"
    return _apply_odata(entities, req["query"], base_url)

# on_get_message returns a single message by id.
# GET /v1.0/me/messages/{id} (Bearer)
def on_get_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    msg_id = req["params"].get("id", "")
    mc = store_collection("messages")
    doc = mc.get(msg_id)
    if doc == None:
        return _err("MessageNotFound", 404, "Message '" + msg_id + "' not found.")

    entity = _message_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('me')/messages/$entity"
    return respond(200, entity)

# on_send_mail sends a message (creates it in Sent Items).
# POST /v1.0/me/sendMail (Bearer)
# Body: { message: { subject, body: { content }, toRecipients: [...] } }
# Returns 202 Accepted (Microsoft Graph returns 202 for sendMail).
def on_send_mail(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    message = body.get("message", body)
    subject = message.get("subject", "")
    body_obj = message.get("body", {})
    content = body_obj.get("content", "")
    if content == None:
        content = ""
    content_type = body_obj.get("contentType", "Text")

    recipients = message.get("toRecipients", [])
    to_addrs = []
    for r in recipients:
        ea = r.get("emailAddress", {})
        to_addrs.append(ea.get("address", ""))

    seq = store_kv_incr("graph", "message_seq")
    msg_id = "AAMkAG" + _pad6(seq) + "-sent"

    doc = {
        "id": msg_id,
        "subject": subject,
        "body": {"contentType": content_type, "content": content},
        "from": {"emailAddress": {"address": "alex@mock-tenant.onmicrosoft.com"}},
        "toRecipients": recipients,
        "receivedDateTime": "2024-06-15T10:30:00Z",
        "sentDateTime": "2024-06-15T10:30:00Z",
        "isRead": True,
        "isDraft": False,
        "folder": "sentitems",
    }

    mc = store_collection("messages")
    mc.insert(doc)

    # sendMail returns 202 Accepted with no body.
    return respond(202)

# --- helpers ---

def _message_entity(doc):
    to_addrs = []
    for r in doc.get("toRecipients", []):
        ea = r.get("emailAddress", {})
        to_addrs.append({"emailAddress": ea})

    return {
        "id": doc["id"],
        "subject": doc.get("subject", ""),
        "body": doc.get("body", {"contentType": "Text", "content": ""}),
        "from": doc.get("from", {"emailAddress": {"address": ""}}),
        "toRecipients": to_addrs,
        "receivedDateTime": doc.get("receivedDateTime", ""),
        "sentDateTime": doc.get("sentDateTime", ""),
        "isRead": doc.get("isRead", True),
        "isDraft": doc.get("isDraft", False),
    }

def _seed_inbox():
    mc = store_collection("messages")
    docs = mc.list()
    if len(docs) > 0:
        return
    seed_msgs = [
        {
            "id": "AAMkAG000001-inbox",
            "subject": "Welcome to Microsoft 365",
            "body": {"contentType": "HTML", "content": "<html><body><h2>Welcome!</h2><p>Your account is ready.</p></body></html>"},
            "from": {"emailAddress": {"address": "noreply@microsoft.com"}},
            "toRecipients": [{"emailAddress": {"address": "alex@mock-tenant.onmicrosoft.com"}}],
            "receivedDateTime": "2024-06-10T08:00:00Z",
            "sentDateTime": "2024-06-10T08:00:00Z",
            "isRead": True,
            "isDraft": False,
            "folder": "inbox",
        },
        {
            "id": "AAMkAG000002-inbox",
            "subject": "Team standup notes",
            "body": {"contentType": "Text", "content": "Notes from today's standup: all on track."},
            "from": {"emailAddress": {"address": "brenda@mock-tenant.onmicrosoft.com"}},
            "toRecipients": [{"emailAddress": {"address": "alex@mock-tenant.onmicrosoft.com"}}],
            "receivedDateTime": "2024-06-12T09:30:00Z",
            "sentDateTime": "2024-06-12T09:30:00Z",
            "isRead": False,
            "isDraft": False,
            "folder": "inbox",
        },
        {
            "id": "AAMkAG000003-inbox",
            "subject": "Q3 Roadmap Review",
            "body": {"contentType": "Text", "content": "Please review the Q3 roadmap before Friday."},
            "from": {"emailAddress": {"address": "charlie@mock-tenant.onmicrosoft.com"}},
            "toRecipients": [{"emailAddress": {"address": "alex@mock-tenant.onmicrosoft.com"}}],
            "receivedDateTime": "2024-06-14T14:00:00Z",
            "sentDateTime": "2024-06-14T14:00:00Z",
            "isRead": True,
            "isDraft": False,
            "folder": "inbox",
        },
    ]
    for m in seed_msgs:
        mc.insert(m)
