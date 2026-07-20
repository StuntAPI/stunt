# Ticket handlers — Zendesk REST v2 tickets with cursor pagination.
#
# GET    /api/v2/tickets              -> list (meta.has_more + links.next)
# POST   /api/v2/tickets              -> create ({ticket:{...}})
# GET    /api/v2/tickets/{id}         -> get single ticket
# PUT    /api/v2/tickets/{id}         -> update ticket
# POST   /api/v2/tickets/{id}/comments-> add comment
# GET    /api/v2/tickets/{id}/comments-> list comments
# POST   /api/v2/tickets/{id}/tags    -> set tags
# GET    /api/v2/search.json?query=   -> search
# GET    /api/v2/requests             -> end-user requests
# GET    /api/v2/suspended_tickets    -> suspended tickets

# Shared helpers from lib.star.

def on_list(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("tickets")
    docs = col.list()

    page_size = _to_int(_get_query(req, "per_page", "100"))
    if page_size <= 0:
        page_size = 100
    page = _to_int(_get_query(req, "page", "1"))
    if page <= 0:
        page = 1
    offset = (page - 1) * page_size

    total = len(docs)
    end = offset + page_size
    if end > total:
        end = total
    if offset > total:
        offset = total

    paged = docs[offset:end]

    tickets = []
    for d in paged:
        tickets.append(_ticket_shape(d))

    resp = {
        "tickets": tickets,
        "meta": {"has_more": _has_more(total, page_size, offset)},
    }

    if _has_more(total, page_size, offset):
        resp["links"] = {"next": _next_link("/api/v2/tickets", end, page_size)}
    else:
        resp["links"] = {"next": None}

    return respond(200, resp)

def on_create(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    body = _get_body(req)
    ticket_data = body.get("ticket", body)

    subject = ticket_data.get("subject", "")
    if subject == None:
        subject = ""
    comment = ticket_data.get("comment", {})
    if comment == None:
        comment = {}
    body_text = comment.get("body", "")
    if body_text == None:
        body_text = ""
    requester = ticket_data.get("requester", {})
    if requester == None:
        requester = {}

    ticket_id = _next_id("ticket")

    doc = {
        "id": ticket_id,
        "subject": subject,
        "status": ticket_data.get("status", "open"),
        "requester_id": str(requester.get("id", "")) if requester.get("id") else _next_id("user"),
        "assignee_id": ticket_data.get("assignee_id", None),
        "created_at": _now(),
        "updated_at": _now(),
        "description": body_text,
    }

    col = store_collection("tickets")
    col.insert(doc)

    # Store the initial comment.
    cc = store_collection("comments")
    cmt_seq = store_kv_incr("zendesk", "comment_seq")
    cc.insert({
        "id": str(cmt_seq + 1),
        "ticket_id": ticket_id,
        "body": body_text,
        "author_id": doc["requester_id"],
        "created_at": _now(),
        "public": True,
    })

    return respond(201, {"ticket": _ticket_shape(doc)})

def on_get(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    ticket_id = req["params"].get("id", "")
    col = store_collection("tickets")
    doc = col.get(ticket_id)
    if doc == None:
        return _zd_error(404, "RecordNotFound", "Ticket not found")

    return respond(200, {"ticket": _ticket_shape(doc)})

def on_update(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    ticket_id = req["params"].get("id", "")
    col = store_collection("tickets")
    doc = col.get(ticket_id)
    if doc == None:
        return _zd_error(404, "RecordNotFound", "Ticket not found")

    body = _get_body(req)
    ticket_data = body.get("ticket", body)

    updated = {}
    for k in doc:
        updated[k] = doc[k]
    if "status" in ticket_data:
        updated["status"] = ticket_data.get("status")
    if "subject" in ticket_data:
        updated["subject"] = ticket_data.get("subject")
    if "assignee_id" in ticket_data:
        updated["assignee_id"] = ticket_data.get("assignee_id")
    updated["updated_at"] = _now()

    # Handle comment in update.
    comment = ticket_data.get("comment", None)
    if comment != None:
        body_text = comment.get("body", "")
        if body_text == None:
            body_text = ""
        cc = store_collection("comments")
        cmt_seq = store_kv_incr("zendesk", "comment_seq")
        cc.insert({
            "id": str(cmt_seq + 1),
            "ticket_id": ticket_id,
            "body": body_text,
            "author_id": "201",
            "created_at": _now(),
            "public": comment.get("public", True),
        })

    col.update(ticket_id, updated)
    return respond(200, {"ticket": _ticket_shape(updated)})

def on_add_comment(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    ticket_id = req["params"].get("id", "")
    col = store_collection("tickets")
    doc = col.get(ticket_id)
    if doc == None:
        return _zd_error(404, "RecordNotFound", "Ticket not found")

    body = _get_body(req)
    comment_data = body.get("ticket", body)
    comment = comment_data.get("comment", comment_data)
    if comment == None:
        comment = {}
    body_text = comment.get("body", "")
    if body_text == None:
        body_text = ""
    is_public = comment.get("public", True)
    if is_public == None:
        is_public = True

    cmt_seq = store_kv_incr("zendesk", "comment_seq")
    cmt_id = str(cmt_seq + 1)

    cmt_doc = {
        "id": cmt_id,
        "ticket_id": ticket_id,
        "body": body_text,
        "author_id": "201",
        "created_at": _now(),
        "public": is_public,
    }

    cc = store_collection("comments")
    cc.insert(cmt_doc)

    # Update ticket's updated_at.
    updated = {}
    for k in doc:
        updated[k] = doc[k]
    updated["updated_at"] = _now()
    col.update(ticket_id, updated)

    return respond(201, {"audit": {"events": [{"type": "Comment", "body": body_text, "public": is_public}]}})

def on_list_comments(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    ticket_id = req["params"].get("id", "")
    cc = store_collection("comments")
    docs = cc.list()

    comments = []
    for d in docs:
        if d.get("ticket_id", "") == ticket_id:
            comments.append({
                "id": d.get("id", ""),
                "ticket_id": d.get("ticket_id", ""),
                "body": d.get("body", ""),
                "author_id": d.get("author_id", ""),
                "created_at": d.get("created_at", _now()),
                "public": d.get("public", True),
            })

    return respond(200, {"comments": comments})

def on_set_tags(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    ticket_id = req["params"].get("id", "")
    body = _get_body(req)
    tag_list = body.get("tags", [])
    if tag_list == None:
        tag_list = []

    tc = store_collection("tags")
    tag_key = "ticket_" + ticket_id
    existing = tc.get(tag_key)
    tag_doc = {
        "id": tag_key,
        "ticket_id": ticket_id,
        "tags": tag_list,
    }
    if existing == None:
        tc.insert(tag_doc)
    else:
        tc.update(tag_key, tag_doc)

    return respond(200, {"tags": tag_list})

def on_search(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    query = _get_query(req, "query", "")
    col = store_collection("tickets")
    docs = col.list()

    results = []
    for d in docs:
        if query == "":
            results.append(_ticket_shape(d))
        else:
            subject = d.get("subject", "")
            desc = d.get("description", "")
            if _contains(subject.lower(), query.lower()) or _contains(desc.lower(), query.lower()):
                results.append(_ticket_shape(d))

    return respond(200, {
        "results": results,
        "meta": {"has_more": False},
        "links": {"next": None},
    })

def on_list_requests(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("tickets")
    docs = col.list()

    requests = []
    for d in docs:
        requests.append({
            "id": d.get("id", ""),
            "subject": d.get("subject", ""),
            "status": d.get("status", "open"),
            "requester_id": d.get("requester_id", ""),
            "created_at": d.get("created_at", _now()),
            "updated_at": d.get("updated_at", _now()),
            "description": d.get("description", ""),
        })

    return respond(200, {"requests": requests})

def on_list_suspended(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    return respond(200, {"suspended_tickets": []})

# _contains returns True if haystack contains needle (case-sensitive).
def _contains(haystack, needle):
    if len(needle) == 0:
        return True
    for i in range(len(haystack) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if haystack[i + j] != needle[j]:
                match = False
                break
        if match:
            return True
    return False
