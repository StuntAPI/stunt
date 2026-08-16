# Microsoft Graph v1.0 — Outlook mail handlers.
#
# GET   /v1.0/me/mailFolders               → mail folders
# GET   /v1.0/me/mailFolders/{id}/messages → folder-scoped message list (OData)
# GET   /v1.0/me/messages                  → list messages (OData)
# POST  /v1.0/me/messages                  → create a draft (201, STATEFUL)
# GET   /v1.0/me/messages/{id}             → get a single message
# PATCH /v1.0/me/messages/{id}             → update (isRead, flag, ...) (STATEFUL)
# DELETE /v1.0/me/messages/{id}            → delete a message (204)
# POST  /v1.0/me/messages/{id}/send        → send a draft (202, STATEFUL)
# POST  /v1.0/me/sendMail                  → send a message (202, STATEFUL)
#
# Messages are STATEFUL: drafts created via POST /me/messages appear in the
# Drafts folder, sending one (sendMail or the draft send action) moves it to
# Sent Items, and PATCHes (isRead / flag) persist — enabling full
# create → flag/mark-read → send → list round-trip testing.

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

    # Pagination: $top = page size, $skip = plain numeric offset.
    base_url = "https://graph.microsoft.com/v1.0/me/mailFolders"
    top = _to_int(req["query"].get("$top", ""))
    page, next_cursor, ok = _list_page(folders, req["query"])
    if not ok:
        return _err("invalidRequest", 400, "$top and $skip must be non-negative integers.")

    envelope = {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users('me')/mailFolders",
        "value": page,
    }
    if next_cursor != None and next_cursor != "":
        envelope["@odata.nextLink"] = _odata_link(base_url, top, next_cursor)
    return respond(200, envelope)

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

# on_list_folder_messages returns the messages of one mail folder.
# GET /v1.0/me/mailFolders/{id}/messages (Bearer)
# Well-known folder ids (inbox, sentitems, drafts, junkemail) address the
# default folders; an unknown folder id is Graph's 404 ErrorFolderNotFound.
def on_list_folder_messages(req):
    err = _require_bearer(req)
    if err != None:
        return err

    folder_id = req["params"].get("id", "")
    if folder_id not in ["inbox", "sentitems", "drafts", "junkemail"]:
        return _err("ErrorFolderNotFound", 404, "The folder '" + folder_id + "' was not found.")

    _seed_inbox()
    mc = store_collection("messages")
    entities = []
    for m in mc.list():
        if m.get("folder", "") == folder_id:
            entities.append(_message_entity(m))

    base_url = "https://graph.microsoft.com/v1.0/me/mailFolders/" + folder_id + "/messages"
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
        return _err("ErrorItemNotFound", 404, "The specified object was not found in the store.")

    entity = _message_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('me')/messages/$entity"
    return respond(200, entity)

# on_update_message partially updates a message — the client-visible use is
# isRead toggling and followup flags, but subject/body/categories/importance
# patch too, like real Graph PATCH /me/messages/{id}.
# PATCH /v1.0/me/messages/{id} (Bearer) → 200 with the updated message
def on_update_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    msg_id = req["params"].get("id", "")
    mc = store_collection("messages")
    doc = mc.get(msg_id)
    if doc == None:
        return _err("ErrorItemNotFound", 404, "The specified object was not found in the store.")

    body = req["body"]
    if body == None:
        body = {}
    for k in ["isRead", "flag", "subject", "body", "categories", "importance"]:
        if body.get(k) != None:
            doc[k] = body[k]
    mc.update(msg_id, doc)

    # Notify subscriptions on me/messages (fire-and-forget).
    _notify_subscriptions("updated", "me/messages", "#Microsoft.Graph.Message", msg_id)

    entity = _message_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('me')/messages/$entity"
    return respond(200, entity)

# on_delete_message deletes a single message by id.
# DELETE /v1.0/me/messages/{id} (Bearer) → 204 No Content
def on_delete_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    msg_id = req["params"].get("id", "")
    mc = store_collection("messages")
    doc = mc.get(msg_id)
    if doc == None:
        return _err("ErrorItemNotFound", 404, "The specified object was not found in the store.")

    mc.delete(msg_id)
    return respond(204)

# on_create_draft_message creates a draft message in the Drafts folder
# (isDraft: true, folder: "drafts"). Real Graph returns the draft with
# receivedDateTime null and sentDateTime null.
# POST /v1.0/me/messages (Bearer) → 201 with the draft message
def on_create_draft_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    doc = _draft_from_body(req["body"])
    mc = store_collection("messages")
    mc.insert(doc)

    # Notify subscriptions on me/messages (fire-and-forget).
    _notify_subscriptions("created", "me/messages", "#Microsoft.Graph.Message", doc["id"])

    entity = _message_entity(doc)
    entity["@odata.context"] = "https://graph.microsoft.com/v1.0/$metadata#users('me')/messages/$entity"
    return respond(201, entity)

# on_send_draft_message sends an existing draft (POST /me/messages/{id}/send):
# the message leaves Drafts, lands in Sent Items with isDraft false and a
# sentDateTime stamp. Sending a non-draft is Graph's 400; an unknown id 404s.
# POST /v1.0/me/messages/{id}/send (Bearer) → 202 Accepted, no body
def on_send_draft_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    msg_id = req["params"].get("id", "")
    mc = store_collection("messages")
    doc = mc.get(msg_id)
    if doc == None:
        return _err("ErrorItemNotFound", 404, "The specified object was not found in the store.")

    if doc.get("isDraft", False) != True:
        return _err("ErrorInvalidOperation", 400, "Only draft messages can be sent.")

    doc["isDraft"] = False
    doc["folder"] = "sentitems"
    doc["sentDateTime"] = clock.now_rfc3339()
    mc.update(msg_id, doc)

    # Notify subscriptions on me/messages (fire-and-forget).
    _notify_subscriptions("updated", "me/messages", "#Microsoft.Graph.Message", msg_id)

    # Graph's send action returns 202 Accepted with no body.
    return respond(202)

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

    # Notify subscriptions on me/messages (fire-and-forget).
    _notify_subscriptions("created", "me/messages", "#Microsoft.Graph.Message", msg_id)

    # sendMail returns 202 Accepted with no body.
    return respond(202)

# --- helpers ---

# _draft_from_body builds a Drafts-folder message doc from a POST /me/messages
# body ({subject, body, toRecipients, ...}). Drafts carry null received/sent
# timestamps, exactly like real Graph.
def _draft_from_body(body):
    if body == None:
        body = {}
    body_obj = body.get("body", {})
    content = body_obj.get("content", "")
    if content == None:
        content = ""
    seq = store_kv_incr("graph", "message_seq")
    return {
        "id": "AAMkAG" + _pad6(seq) + "-draft",
        "subject": body.get("subject", ""),
        "body": {"contentType": body_obj.get("contentType", "Text"), "content": content},
        "from": {"emailAddress": {"address": "alex@mock-tenant.onmicrosoft.com"}},
        "toRecipients": body.get("toRecipients", []),
        "receivedDateTime": None,
        "sentDateTime": None,
        "isRead": True,
        "isDraft": True,
        "flag": {"flagStatus": "notFlagged"},
        "categories": [],
        "folder": "drafts",
    }

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
        "flag": doc.get("flag", {"flagStatus": "notFlagged"}),
    }

def _seed_inbox():
    # Guard on a KV flag, not on "collection is empty": a client that sends
    # mail (or creates a draft) before its first list must not suppress the
    # seeded inbox.
    if store_kv_get("graph", "inbox_seeded") == "yes":
        return
    store_kv_set("graph", "inbox_seeded", "yes")
    mc = store_collection("messages")
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
