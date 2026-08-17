# Field handlers — list-scoped custom contact fields (/lists/{list_id}/fields).
#
#   POST   /lists/{list_id}/fields            create field (201, 409 on dup tag)
#   PUT    /lists/{list_id}/fields/{tag}      update field (200)
#   DELETE /lists/{list_id}/fields/{tag}      delete field (204)
#
# A field is {label, tag, type, fallback?, choices?} where type is one of
# text | number | date | choice_single | choice_multiple (choice fields
# require a choices array). Fields live on the list document and are
# returned inline by the List-get shape.
#
# Shared helpers are preloaded from scripts/lib.star.

# on_create_field answers POST /lists/{list_id}/fields.
def on_create_field(req):
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

    bad = _validate_field(body)
    if bad != None:
        return bad

    tag = body.get("tag", "")
    if _find_field(lst, tag) != None:
        return _conflict()

    # Persist BEFORE emitting.
    field = _field_from(body)
    lst["fields"].append(field)
    lst["last_updated_at"] = _iso_now()
    lc.update(list_id, lst)
    _emit("field.created", {"list_id": list_id, "field": field})
    return respond(201, field)

# on_update_field answers PUT /lists/{list_id}/fields/{tag}.
def on_update_field(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    tag = _param(req, "tag")
    lc = store_collection("lists")
    lst = lc.get(list_id)
    if lst == None:
        return _not_found()
    existing = _find_field(lst, tag)
    if existing == None:
        return _not_found()

    body = _parse_body(req)
    if body == None:
        return _bad_request()

    bad = _validate_field(body)
    if bad != None:
        return bad

    new_tag = body.get("tag", "")
    if new_tag != tag and _find_field(lst, new_tag) != None:
        return _conflict()

    # Persist the replacement field in place, keeping the array order.
    updated = _field_from(body)
    out = []
    for f in lst.get("fields", []):
        if f.get("tag", "") == tag:
            out.append(updated)
        else:
            out.append(f)
    lst["fields"] = out
    lst["last_updated_at"] = _iso_now()
    lc.update(list_id, lst)

    # A renamed field re-keys its values on every contact of the list.
    if new_tag != tag:
        cc = store_collection("contacts")
        for c in _list_contacts(list_id):
            cfields = c.get("fields", {})
            if cfields.get(tag, None) != None:
                cfields[new_tag] = cfields.pop(tag)
                c["fields"] = cfields
                c["last_updated_at"] = _iso_now()
                cc.update(c.get("id", ""), c)

    _emit("field.updated", {"list_id": list_id, "field": updated})
    return respond(200, updated)

# on_delete_field answers DELETE /lists/{list_id}/fields/{tag} with 204.
def on_delete_field(req):
    err = _require_auth(req)
    if err != None:
        return err

    list_id = _param(req, "list_id")
    tag = _param(req, "tag")
    lc = store_collection("lists")
    lst = lc.get(list_id)
    if lst == None or _find_field(lst, tag) == None:
        return _not_found()

    out = []
    for f in lst.get("fields", []):
        if f.get("tag", "") != tag:
            out.append(f)
    lst["fields"] = out
    lst["last_updated_at"] = _iso_now()
    lc.update(list_id, lst)

    # Remove the deleted field's values from every contact.
    cc = store_collection("contacts")
    for c in _list_contacts(list_id):
        cfields = c.get("fields", {})
        if cfields.get(tag, None) != None:
            cfields.pop(tag, None)
            c["fields"] = cfields
            c["last_updated_at"] = _iso_now()
            cc.update(c.get("id", ""), c)

    _emit("field.deleted", {"list_id": list_id, "tag": tag})
    return respond(204)

# ============================================================================
# INTERNALS
# ============================================================================

# _find_field returns the field with the given tag on a list doc, or None.
def _find_field(lst, tag):
    for f in lst.get("fields", []):
        if f.get("tag", "") == tag:
            return f
    return None

# _validate_field checks the oneOf create/update shape: label/tag/type are
# required; choice_* types require a non-empty choices array. Returns the
# 422 response or None.
def _validate_field(body):
    errs = []
    label = _str_or_none(body.get("label", None))
    if label == None or label == "":
        errs.append(_verr("/label", "This value should not be blank."))
    tag = _str_or_none(body.get("tag", None))
    if tag == None or tag == "":
        errs.append(_verr("/tag", "This value should not be blank."))
    ftype = _str_or_none(body.get("type", None))
    if ftype == None or ftype not in _FIELD_TYPES:
        errs.append(_verr("/type", "The value you selected is not a valid choice."))
    elif _is_choice_type(ftype):
        choices = body.get("choices", None)
        if _is_str_list(choices) == False or len(choices) == 0:
            errs.append(_verr("/choices", "This value should not be blank."))
    if len(errs) > 0:
        return _unprocessable(errs)
    return None

# _is_choice_type reports whether the field type takes a choices array.
def _is_choice_type(ftype):
    return ftype == "choice_single" or ftype == "choice_multiple"

# _field_from projects a request body into the stored/public field shape.
def _field_from(body):
    f = {
        "label": body.get("label", ""),
        "tag": body.get("tag", ""),
        "type": body.get("type", ""),
    }
    fallback = body.get("fallback", None)
    if fallback != None:
        f["fallback"] = fallback
    if _is_choice_type(body.get("type", "")):
        f["choices"] = body.get("choices", [])
    return f
