# Contact handlers — the list-scoped /lists/{list_id}/contacts surface.
#
#   GET    /lists/{list_id}/contacts                      list + filter
#   POST   /lists/{list_id}/contacts                      create (201)
#   PUT    /lists/{list_id}/contacts                      create-or-update (upsert)
#   PUT    /lists/{list_id}/contacts/batch                update many
#   GET    /lists/{list_id}/contacts/{contact_id}         get
#   PUT    /lists/{list_id}/contacts/{contact_id}         update (status/fields/tags)
#   DELETE /lists/{list_id}/contacts/{contact_id}         delete (204)
#
# Status lifecycle (verified from the v2 docs + double-opt-in help article):
#   - create with no explicit status on a double-opt-in list → "pending"
#     (the contact must confirm before becoming subscribed)
#   - create with no explicit status on a single-opt-in list → "subscribed"
#   - an explicit status member is honoured (pending/subscribed/unsubscribed)
#   - unsubscribe = PUT the status to "unsubscribed"; resubscribe = PUT it
#     back to "subscribed"
#
# Shared helpers are preloaded from scripts/lib.star.

# on_list_contacts answers GET /lists/{list_id}/contacts.
#
# Query params (from the v2 spec): limit, starting_after, status
# (subscribed|unsubscribed|pending), tag, created_at.lte/gte,
# last_updated_at.lte/gte.
def on_list_contacts(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    if _get_list(list_id) == None:
        return _not_found()

    q = _query(req)
    docs = _list_contacts(list_id)

    # tag selects contacts carrying a tag. tags is an ARRAY on the contact,
    # which a query_select triple cannot express, so it is applied as a
    # manual pass; every other filter maps to [field, op, value] triples.
    tag = q.get("tag", "")
    if tag != None and tag != "":
        kept = []
        for d in docs:
            if tag in d.get("tags", []):
                kept.append(d)
        docs = kept

    f = []
    status = q.get("status", "")
    if status != None and status != "":
        f.append(["status", "=", status])
    clte = q.get("created_at.lte", "")
    if clte != None and clte != "":
        f.append(["created_at", "<=", clte])
    cgte = q.get("created_at.gte", "")
    if cgte != None and cgte != "":
        f.append(["created_at", ">=", cgte])
    ulte = q.get("last_updated_at.lte", "")
    if ulte != None and ulte != "":
        f.append(["last_updated_at", "<=", ulte])
    ugte = q.get("last_updated_at.gte", "")
    if ugte != None and ugte != "":
        f.append(["last_updated_at", ">=", ugte])
    docs = query_select(docs, f if len(f) > 0 else None, None, None, None, None, None)

    return _paginated(req, "/lists/" + list_id + "/contacts",
                      [_present_contact(d) for d in docs])

# on_create_contact answers POST /lists/{list_id}/contacts (201). Body:
# email_address (required), fields (object), tags (array of strings),
# status (optional enum).
def on_create_contact(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    lc = store_collection("lists")
    lst = lc.get(list_id)
    if lst == None:
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    errs = []
    email = _str_or_none(body.get("email_address", None))
    if email == None or email == "":
        errs.append(_verr("/email_address", "This value should not be blank."))
    elif _email_ok(email) == False:
        errs.append(_verr("/email_address", "This value is not a valid email address."))

    status = body.get("status", None)
    if status != None and _status_ok(status) == False:
        errs.append(_verr("/status", "The value you selected is not a valid choice."))

    tags = body.get("tags", None)
    if tags == None:
        tags = []
    if _is_str_list(tags) == False:
        errs.append(_verr("/tags", "This value should be of type array<string>."))
        tags = []
    fields = body.get("fields", None)
    if fields != None and type(fields) != "dict":
        errs.append(_verr("/fields", "This value should be of type object."))
        fields = None

    if len(errs) > 0:
        return _unprocessable(errs)

    # Adding an email that is already on the list answers 409 conflict.
    existing = _find_contact(list_id, email)
    if existing != None:
        return _conflict()

    if status == None:
        # Double opt-in lists hold new contacts at "pending" until they
        # confirm; single opt-in lists subscribe immediately.
        if lst.get("double_opt_in", False):
            status = "pending"
        else:
            status = "subscribed"

    doc = {
        "id": _row_id(list_id, _contact_id(email)),
        "contact_id": _contact_id(email),
        "list_id": list_id,
        "email_address": email,
        "fields": _clean_fields(fields),
        "tags": _dedupe_tags(tags),
        "status": status,
        "created_at": _iso_now(),
        "last_updated_at": _iso_now(),
    }
    store_collection("contacts").insert(doc)
    _emit("contact.created", _present_contact(doc))
    return respond(201, _present_contact(doc))

# on_upsert_contact answers PUT /lists/{list_id}/contacts — the documented
# create-or-update endpoint keyed on email_address (200 in both cases; the
# tags member is an object of tag → bool here, unlike the array on POST).
def on_upsert_contact(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    if _get_list(list_id) == None:
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    errs = []
    email = _str_or_none(body.get("email_address", None))
    if email == None or email == "":
        errs.append(_verr("/email_address", "This value should not be blank."))
    elif _email_ok(email) == False:
        errs.append(_verr("/email_address", "This value is not a valid email address."))
    status = body.get("status", None)
    if status != None and _status_ok(status) == False:
        errs.append(_verr("/status", "The value you selected is not a valid choice."))
    tags = body.get("tags", None)
    if tags != None and type(tags) != "dict":
        errs.append(_verr("/tags", "This value should be of type object."))
        tags = None
    fields = body.get("fields", None)
    if fields != None and type(fields) != "dict":
        errs.append(_verr("/fields", "This value should be of type object."))
        fields = None
    if len(errs) > 0:
        return _unprocessable(errs)

    existing = _find_contact(list_id, email)
    if existing != None:
        ok, payload = _apply_contact_update(list_id, existing, body)
        if ok:
            return respond(200, payload)
        return payload

    if status == None:
        status = "subscribed"
    doc = {
        "id": _row_id(list_id, _contact_id(email)),
        "contact_id": _contact_id(email),
        "list_id": list_id,
        "email_address": email,
        "fields": _clean_fields(fields),
        "tags": _dedupe_tags(_tags_from_map(tags, None)),
        "status": status,
        "created_at": _iso_now(),
        "last_updated_at": _iso_now(),
    }
    store_collection("contacts").insert(doc)
    _emit("contact.created", _present_contact(doc))
    return respond(200, _present_contact(doc))

# on_batch_update_contacts answers PUT /lists/{list_id}/contacts/batch.
# Body: {"contacts": [{id, email_address?, fields?, tags?, status?}, ...]}.
# Response (v2 schema): {"success": [{success: true, data: contact}],
# "errors": [{success: false, id, type, title, detail, status, data: null}]}.
def on_batch_update_contacts(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    if _get_list(list_id) == None:
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    contacts = body.get("contacts", None)
    if contacts == None or type(contacts) != "list":
        return _unprocessable([_verr("/contacts", "This value should not be blank.")])

    cc = store_collection("contacts")
    success = []
    errors = []
    for item in contacts:
        if item == None or type(item) != "dict":
            errors.append(_batch_error("", "invalid_body", "Invalid body",
                                       "The request body is invalid.", 400))
            continue
        cid = item.get("id", None)
        if cid == None or type(cid) != "string" or cid == "":
            errors.append(_batch_error("", "invalid_body", "Invalid body",
                                       "The contact id is missing.", 400))
            continue
        doc = cc.get(_row_id(list_id, cid))
        if doc == None or doc.get("list_id", "") != list_id:
            errors.append(_batch_error(cid, "not_found", "Resource not found",
                                       "Resource not found.", 404))
            continue
        ok, payload = _apply_contact_update(list_id, doc, item)
        if ok:
            success.append({"success": True, "data": payload})
        else:
            # payload is the RFC 7807 error response for this row.
            errors.append(_batch_error(cid, "unprocessable_content",
                                       payload.get("title", "An error occurred."),
                                       payload.get("detail", "Unprocessable content."),
                                       payload.get("status", 422)))
    return respond(200, {"success": success, "errors": errors})

# on_get_contact answers GET /lists/{list_id}/contacts/{contact_id}. The
# contact id is the 32-hex id derived from the email address.
def on_get_contact(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    if _get_list(list_id) == None:
        return _not_found()

    contact_id = _param(req, "contact_id")
    doc = store_collection("contacts").get(_row_id(list_id, contact_id))
    if doc == None or doc.get("list_id", "") != list_id:
        return _not_found()
    return respond(200, _present_contact(doc))

# on_update_contact answers PUT /lists/{list_id}/contacts/{contact_id}.
# Members are all optional; tags is an object of tag → bool (true adds,
# false removes, unreferenced tags untouched).
def on_update_contact(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    if _get_list(list_id) == None:
        return _not_found()

    contact_id = _param(req, "contact_id")
    cc = store_collection("contacts")
    doc = cc.get(_row_id(list_id, contact_id))
    if doc == None or doc.get("list_id", "") != list_id:
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    ok, payload = _apply_contact_update(list_id, doc, body)
    if ok:
        return respond(200, payload)
    return payload

# on_delete_contact answers DELETE /lists/{list_id}/contacts/{contact_id}
# with 204 No Content.
def on_delete_contact(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    if _get_list(list_id) == None:
        return _not_found()

    contact_id = _param(req, "contact_id")
    cc = store_collection("contacts")
    doc = cc.get(_row_id(list_id, contact_id))
    if doc == None or doc.get("list_id", "") != list_id:
        return _not_found()

    cc.delete(_row_id(list_id, contact_id))
    _emit("contact.deleted", _present_contact(doc))
    return respond(204)

# ============================================================================
# INTERNALS
# ============================================================================

# _apply_contact_update applies a PUT body to an existing contact doc,
# persists it, and emits exactly one event per actual transition (status
# changes emit contact.status.changed; any change emits contact.updated).
# Returns (True, presented_contact) on success, or (False, error_response)
# when the body fails validation.
def _apply_contact_update(list_id, doc, body):
    errs = []
    email = _str_or_none(body.get("email_address", None))
    if email != None and email != "" and _email_ok(email) == False:
        errs.append(_verr("/email_address", "This value is not a valid email address."))
    status = body.get("status", None)
    if status != None and _status_ok(status) == False:
        errs.append(_verr("/status", "The value you selected is not a valid choice."))
    fields = body.get("fields", None)
    if fields != None and type(fields) != "dict":
        errs.append(_verr("/fields", "This value should be of type object."))
    tags = body.get("tags", None)
    if tags != None and type(tags) != "dict":
        errs.append(_verr("/tags", "This value should be of type object."))
    if len(errs) > 0:
        return False, _unprocessable(errs)

    cc = store_collection("contacts")
    old_id = doc.get("id", "")
    old_status = doc.get("status", "")
    changed = False

    if email != None and email != "" and email != doc.get("email_address", ""):
        # The contact id IS the hash of the email address, so a changed
        # address rekeys the row. The destination must be free on THIS list
        # (409 otherwise) — checked BEFORE any mutation so the original row
        # is never destroyed by a failed rekey.
        new_row = _row_id(list_id, _contact_id(email))
        if store_collection("contacts").get(new_row) != None:
            return False, _conflict()
        doc["email_address"] = email
        doc["id"] = new_row
        doc["contact_id"] = _contact_id(email)
        changed = True

    if fields != None:
        merged = _merge_fields(doc.get("fields", {}), fields)
        if merged != doc.get("fields", {}):
            doc["fields"] = merged
            changed = True

    if tags != None:
        new_tags = _tags_from_map(tags, doc)
        if new_tags != doc.get("tags", []):
            doc["tags"] = new_tags
            changed = True

    if status != None and status != old_status:
        doc["status"] = status
        changed = True

    doc["last_updated_at"] = _iso_now()

    # Persist BEFORE emitting. A rekeyed row is written as delete + insert.
    if doc.get("id", "") != old_id:
        cc.delete(old_id)
        cc.insert(doc)
    else:
        cc.update(old_id, doc)

    if status != None and status != old_status:
        _emit("contact.status.changed", _present_contact(doc))
    if changed:
        _emit("contact.updated", _present_contact(doc))
    return True, _present_contact(doc)

# _merge_fields applies the fields object onto the stored fields: a value of
# None REMOVES the field (documented "Unset" behaviour), anything else sets
# it. Field values are kept exactly as provided (text/date/number/choices).
def _merge_fields(current, incoming):
    out = {}
    for k in current:
        out[k] = current[k]
    for k in incoming:
        v = incoming[k]
        if v == None:
            out.pop(k, None)
        else:
            out[k] = v
    return out

# _clean_fields normalizes an optional create-time fields member to a dict.
def _clean_fields(v):
    if v == None or type(v) != "dict":
        return {}
    out = {}
    for k in v:
        if v[k] != None:
            out[k] = v[k]
    return out

# _tags_from_map applies the PUT tags object (tag → bool) to a contact's
# current tag list: true appends, false removes, unreferenced tags untouched.
def _tags_from_map(tags, doc):
    out = []
    current = []
    if doc != None:
        current = doc.get("tags", [])
    for t in current:
        out.append(t)
    if tags == None or type(tags) != "dict":
        return out
    for name in tags:
        if tags[name]:
            if name not in out:
                out.append(name)
        else:
            kept = []
            for t in out:
                if t != name:
                    kept.append(t)
            out = kept
    return out

# _dedupe_tags removes duplicate tag names preserving first occurrence.
def _dedupe_tags(tags):
    out = []
    for t in tags:
        if t not in out:
            out.append(t)
    return out

# _batch_error builds one per-item error entry for the batch envelope.
def _batch_error(cid, etype, title, detail, status):
    return {
        "success": False,
        "id": cid,
        "type": etype,
        "title": title,
        "detail": detail,
        "status": status,
        "data": None,
    }
