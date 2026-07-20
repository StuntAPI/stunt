# Message handlers — list, get, send, insert, modify, trash, attachments.
#
# GET    /gmail/v1/users/{userId}/messages                         → list
# GET    /gmail/v1/users/{userId}/messages/{messageId}              → get (format: full|metadata|raw)
# POST   /gmail/v1/users/{userId}/messages/send                    → send (raw rfc822)
# POST   /gmail/v1/users/{userId}/messages                         → insert
# POST   /gmail/v1/users/{userId}/messages/{messageId}/modify       → modify labels
# POST   /gmail/v1/users/{userId}/messages/{messageId}/trash        → trash
# POST   /gmail/v1/users/{userId}/messages/batchModify              → batch modify
# GET    /gmail/v1/users/{userId}/messages/{messageId}/attachments/{attachmentId} → attachment
#
# STATEFUL: messages sent via POST appear in the messages list and are
# retrievable with full headers via GET.
#
# Shared helpers are preloaded from scripts/lib.star.

# on_list_messages returns a list of message stubs (id + threadId).
def on_list_messages(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    user_id = req["params"]["userId"]
    q = req["query"].get("q", "")
    max_results = _to_int(req["query"].get("maxResults", "100"))
    if max_results == 0:
        max_results = 100
    label_ids = req["query"].get("labelIds", "")

    mc = store_collection("messages")
    messages = []
    count = 0
    for doc in mc.list():
        # Filter by labelIds if specified.
        if label_ids != "" and label_ids != None:
            doc_labels = doc.get("labelIds", [])
            if label_ids not in doc_labels:
                continue

        # Simple query filtering: check if q appears in snippet or subject.
        if q != "" and q != None:
            snippet = doc.get("snippet", "")
            subject = ""
            for h in doc.get("headers", []):
                if h["name"].lower() == "subject":
                    subject = h["value"]
            if q.lower() not in snippet.lower() and q.lower() not in subject.lower():
                continue

        messages.append({
            "id": doc["id"],
            "threadId": doc["threadId"],
        })
        count = count + 1
        if count >= max_results:
            break

    return respond(200, {
        "messages": messages,
        "resultSizeEstimate": len(messages),
    })

# on_get_message returns a single message with the requested format.
# format=full (default): full payload with headers + parts.
# format=metadata: headers only, no body.
# format=raw: returns the raw base64url rfc822.
def on_get_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    msg_id = req["params"]["messageId"]
    fmt = req["query"].get("format", "full")
    if fmt == None or fmt == "":
        fmt = "full"

    doc = _find_message(msg_id)
    if doc == None:
        return _not_found("Message not found: " + msg_id)

    base = {
        "id": doc["id"],
        "threadId": doc["threadId"],
        "labelIds": doc.get("labelIds", []),
    }

    if fmt == "raw":
        raw = doc.get("raw", "")
        if raw == "":
            # Reconstruct from headers + body.
            raw = _reconstruct_raw(doc)
        base["raw"] = raw
        base["sizeEstimate"] = doc.get("sizeEstimate", len(raw))
        return respond(200, base)

    if fmt == "metadata":
        base["snippet"] = doc.get("snippet", "")
        base["payload"] = {
            "partId": "",
            "headers": doc.get("headers", []),
        }
        base["sizeEstimate"] = doc.get("sizeEstimate", 0)
        base["internalDate"] = doc.get("internalDate", "0")
        base["historyId"] = doc.get("historyId", "0")
        return respond(200, base)

    # Default: full format.
    base["snippet"] = doc.get("snippet", "")
    base["payload"] = doc.get("payload", {
        "partId": "",
        "mimeType": "text/plain",
        "filename": "",
        "headers": doc.get("headers", []),
        "body": {
            "size": len(doc.get("bodyText", "")),
            "data": _b64url_encode(doc.get("bodyText", "")),
        },
    })
    base["sizeEstimate"] = doc.get("sizeEstimate", 0)
    base["internalDate"] = doc.get("internalDate", "0")
    base["historyId"] = doc.get("historyId", "0")
    return respond(200, base)

# on_send_message accepts a raw base64url rfc822 message, decodes it, stores
# it, and returns the message stub with SENT label.
def on_send_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    raw = body.get("raw", "")
    if raw == None or raw == "":
        return _bad_request("Missing raw message content")

    # Parse the raw rfc822 message.
    parsed = _parse_rfc822(raw)
    headers = parsed["headers"]
    body_text = parsed["body"]

    from_addr = _header_value(headers, "From")
    to_addr = _header_value(headers, "To")
    subject = _header_value(headers, "Subject")
    date_str = _header_value(headers, "Date")

    snippet = body_text[:100]
    if len(body_text) > 100:
        snippet = body_text[:100]

    seq = _seq("msg_seq")
    msg_id = _gen_message_id(seq + 1)
    thread_id = _gen_thread_id(seq + 1)

    doc = {
        "id": msg_id,
        "threadId": thread_id,
        "labelIds": ["SENT"],
        "snippet": snippet,
        "historyId": str(2000 + seq),
        "internalDate": str(_gen_internal_date(seq + 1)),
        "sizeEstimate": len(raw),
        "raw": raw,
        "headers": headers,
        "bodyText": body_text,
        "payload": {
            "partId": "",
            "mimeType": "text/plain",
            "filename": "",
            "headers": headers,
            "body": {
                "size": len(body_text),
                "data": _b64url_encode(body_text),
            },
        },
    }

    mc = store_collection("messages")
    mc.insert(doc)

    return respond(200, {
        "id": msg_id,
        "threadId": thread_id,
        "labelIds": ["SENT"],
    })

# on_insert_message inserts a message (similar to send but can set labels).
def on_insert_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    raw = body.get("raw", "")
    if raw == None or raw == "":
        return _bad_request("Missing raw message content")

    parsed = _parse_rfc822(raw)
    headers = parsed["headers"]
    body_text = parsed["body"]

    label_ids = body.get("labelIds", ["INBOX"])
    if label_ids == None:
        label_ids = ["INBOX"]

    snippet = body_text[:100]
    if len(body_text) > 100:
        snippet = body_text[:100]

    seq = _seq("msg_seq")
    msg_id = _gen_message_id(seq + 1)
    thread_id = _gen_thread_id(seq + 1)

    doc = {
        "id": msg_id,
        "threadId": thread_id,
        "labelIds": label_ids,
        "snippet": snippet,
        "historyId": str(3000 + seq),
        "internalDate": str(_gen_internal_date(seq + 1)),
        "sizeEstimate": len(raw),
        "raw": raw,
        "headers": headers,
        "bodyText": body_text,
        "payload": {
            "partId": "",
            "mimeType": "text/plain",
            "filename": "",
            "headers": headers,
            "body": {
                "size": len(body_text),
                "data": _b64url_encode(body_text),
            },
        },
    }

    mc = store_collection("messages")
    mc.insert(doc)

    return respond(200, {
        "id": msg_id,
        "threadId": thread_id,
        "labelIds": label_ids,
    })

# on_modify_message adds/removes labels on a message.
def on_modify_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    msg_id = req["params"]["messageId"]
    body = req["body"]
    if body == None:
        body = {}

    add_labels = body.get("addLabelIds", [])
    if add_labels == None:
        add_labels = []
    remove_labels = body.get("removeLabelIds", [])
    if remove_labels == None:
        remove_labels = []

    doc = _find_message(msg_id)
    if doc == None:
        return _not_found("Message not found: " + msg_id)

    current_labels = doc.get("labelIds", [])
    new_labels = []
    for l in current_labels:
        if l not in remove_labels:
            new_labels.append(l)
    for l in add_labels:
        if l not in new_labels:
            new_labels.append(l)
    doc["labelIds"] = new_labels

    mc = store_collection("messages")
    mc.update(doc["id"], doc)

    return respond(200, {
        "id": doc["id"],
        "threadId": doc["threadId"],
        "labelIds": new_labels,
    })

# on_trash_message moves a message to trash (adds TRASH label).
def on_trash_message(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    msg_id = req["params"]["messageId"]
    doc = _find_message(msg_id)
    if doc == None:
        return _not_found("Message not found: " + msg_id)

    current_labels = doc.get("labelIds", [])
    if "TRASH" not in current_labels:
        current_labels.append("TRASH")
    if "INBOX" in current_labels:
        new_labels = []
        for l in current_labels:
            if l != "INBOX":
                new_labels.append(l)
        current_labels = new_labels
    doc["labelIds"] = current_labels

    mc = store_collection("messages")
    mc.update(doc["id"], doc)

    return respond(200, {
        "id": doc["id"],
        "threadId": doc["threadId"],
        "labelIds": current_labels,
    })

# on_batch_modify modifies labels on multiple messages at once.
def on_batch_modify(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    ids = body.get("ids", [])
    if ids == None:
        ids = []
    add_labels = body.get("addLabelIds", [])
    if add_labels == None:
        add_labels = []
    remove_labels = body.get("removeLabelIds", [])
    if remove_labels == None:
        remove_labels = []

    mc = store_collection("messages")
    for doc in mc.list():
        if doc["id"] not in ids:
            continue
        current_labels = doc.get("labelIds", [])
        new_labels = []
        for l in current_labels:
            if l not in remove_labels:
                new_labels.append(l)
        for l in add_labels:
            if l not in new_labels:
                new_labels.append(l)
        doc["labelIds"] = new_labels
        mc.update(doc["id"], doc)

    return respond(204)

# on_get_attachment returns a message attachment by ID.
def on_get_attachment(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    msg_id = req["params"]["messageId"]
    attachment_id = req["params"]["attachmentId"]

    doc = _find_message(msg_id)
    if doc == None:
        return _not_found("Message not found: " + msg_id)

    # Return a synthetic attachment.
    return respond(200, {
        "attachmentId": attachment_id,
        "size": 42,
        "data": _b64url_encode("Synthetic attachment content for testing."),
    })

# === Helpers ===

# _reconstruct_raw builds a raw base64url rfc822 string from a stored message.
def _reconstruct_raw(doc):
    headers = doc.get("headers", [])
    body_text = doc.get("bodyText", "")
    raw_text = ""
    for h in headers:
        raw_text = raw_text + h["name"] + ": " + h["value"] + "\n"
    raw_text = raw_text + "\n" + body_text
    return _b64url_encode(raw_text)
