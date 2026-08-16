# Persons handlers — Stripe Connect (docs.stripe.com/api/persons).
#
# Persons represent the humans associated with a connected account
# (representative, owners, executives, directors). Stored in the "persons"
# collection, keyed globally by person_* id so both the nested routes
# (/v1/accounts/{id}/persons...) and the shortcut routes (/v1/persons/{id})
# resolve the same doc. Soft delete keeps the doc retrievable-by-id semantics
# simple while hiding it from lists, like every Stripe list.
# Emits person.created / person.updated / person.deleted.
# Shared helpers (_require_auth, _next_id, _not_found, _now, _num,
# _signed_emit, _list_page, _newest_first, _get_query) are in lib.star.

# _person_req_shape is the full real requirements object for a person.
def _person_req_shape(req):
    if req == None:
        req = {}
    return {
        "alternatives": req.get("alternatives", []),
        "current_deadline": req.get("current_deadline", None),
        "currently_due": req.get("currently_due", []),
        "disabled_reason": req.get("disabled_reason", None),
        "errors": req.get("errors", []),
        "eventually_due": req.get("eventually_due", []),
        "past_due": req.get("past_due", []),
        "pending_verification": req.get("pending_verification", []),
    }

# _person_rel_shape renders the relationship object with every documented
# key present.
def _person_rel_shape(rel):
    if rel == None:
        rel = {}
    return {
        "director": rel.get("director", False) == True,
        "executive": rel.get("executive", False) == True,
        "legal_guardian": rel.get("legal_guardian", False) == True,
        "owner": rel.get("owner", False) == True,
        "percent_ownership": rel.get("percent_ownership", None),
        "representative": rel.get("representative", False) == True,
        "title": rel.get("title", None),
    }

# _person_dob_shape normalizes a request dob hash ({day, month, year}).
def _person_dob_shape(dob):
    if dob == None or type(dob) != "dict":
        return None
    return {
        "day": _num(dob.get("day", 0)),
        "month": _num(dob.get("month", 0)),
        "year": _num(dob.get("year", 0)),
    }

# _person_verification is the unverified baseline verification object
# (docs.stripe.com/api/persons/object: status unverified until documents are
# provided through onboarding).
def _person_verification():
    return {
        "additional_document": {"details": None, "details_code": None, "document": None},
        "details": None,
        "details_code": None,
        "document": None,
        "status": "unverified",
    }

# _person_public strips internal keys and renders derived shapes.
def _person_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    out["requirements"] = _person_req_shape(doc.get("requirements", None))
    out["relationship"] = _person_rel_shape(doc.get("relationship", None))
    out["verification"] = doc.get("verification", None)
    if out["verification"] == None:
        out["verification"] = _person_verification()
    return out

# _person_get loads a live (non-deleted) person doc, or None.
def _person_get(id):
    doc = store_collection("persons").get(id)
    if doc == None:
        return None
    if doc.get("_deleted", False) == True:
        return None
    return doc

# _person_body folds the whitelist of updatable person params into the doc.
def _person_body(doc, body):
    if body == None:
        return
    for k in ["first_name", "last_name", "maiden_name", "email", "phone", "gender", "nationality", "political_exposure", "address"]:
        v = body.get(k, None)
        if v != None:
            doc[k] = v
    dob = _person_dob_shape(body.get("dob", None))
    if dob != None:
        doc["dob"] = dob
    rel = body.get("relationship", None)
    if rel != None and type(rel) == "dict":
        cur = doc.get("relationship", None)
        if cur == None:
            cur = {}
        for k in rel:
            cur[k] = rel[k]
        doc["relationship"] = cur
    md = body.get("metadata", None)
    if md != None and type(md) == "dict":
        doc["metadata"] = md

# POST /v1/accounts/{id}/persons — create a person on a connected account.
def on_create_person(req):
    err = _require_auth(req)
    if err != None:
        return err

    acct_id = req["params"]["id"]
    if store_collection("connect_accounts").get(acct_id) == None:
        return _not_found("account", acct_id)

    body = req["body"]
    if body == None:
        body = {}

    doc = {
        "id": _next_id("person"),
        "object": "person",
        "account": acct_id,
        "address": None,
        "created": _now(),
        "dob": _person_dob_shape(body.get("dob", None)),
        "email": body.get("email", None),
        "first_name": body.get("first_name", None),
        "id_number_provided": False,
        "last_name": body.get("last_name", None),
        "metadata": body.get("metadata", {}),
        "nationality": None,
        "phone": None,
        "political_exposure": None,
        "relationship": _person_rel_shape(body.get("relationship", None)),
        "requirements": _person_req_shape(None),
        "ssn_last_4_provided": False,
        "verification": _person_verification(),
        "_deleted": False,
    }
    _person_body(doc, body)
    store_collection("persons").insert(doc)
    _signed_emit("person.created", _person_public(doc))
    return respond(200, _person_public(doc))

# GET /v1/accounts/{id}/persons — list a connected account's persons. Real
# filter: relationship[owner]=true / relationship[representative]=true (form-
# encoded bracket params arrive as literal query keys).
def on_list_persons(req):
    err = _require_auth(req)
    if err != None:
        return err

    acct_id = req["params"]["id"]
    if store_collection("connect_accounts").get(acct_id) == None:
        return _not_found("account", acct_id)

    docs = store_collection("persons").list()
    docs = query_select(docs, [["account", "=", acct_id], ["_deleted", "!=", True]])
    for flag in ["owner", "representative", "executive", "director"]:
        key = "relationship[" + flag + "]"
        v = _get_query(req, key)
        if v == "true" or v == "false":
            want = v == "true"
            keep = []
            for i in range(len(docs)):
                rel = docs[i].get("relationship", None)
                if rel == None:
                    rel = {}
                if (rel.get(flag, False) == True) == want:
                    keep.append(docs[i])
            docs = keep
    docs = _newest_first(docs)

    page, has_more, err2 = _list_page(req, docs, "person")
    if err2 != None:
        return err2
    return respond(200, {"object": "list", "data": [_person_public(d) for d in page], "has_more": has_more, "url": "/v1/accounts/" + acct_id + "/persons"})

# GET /v1/accounts/{id}/persons/{person_id} — retrieve one person (must
# belong to the account in the path).
def on_retrieve_person(req):
    err = _require_auth(req)
    if err != None:
        return err

    acct_id = req["params"]["id"]
    person_id = req["params"]["person_id"]
    doc = _person_get(person_id)
    if doc == None or doc.get("account", None) != acct_id:
        return _not_found("person", person_id)
    return respond(200, _person_public(doc))

# POST /v1/accounts/{id}/persons/{person_id} — update a person.
def on_update_person(req):
    err = _require_auth(req)
    if err != None:
        return err

    acct_id = req["params"]["id"]
    person_id = req["params"]["person_id"]
    doc = _person_get(person_id)
    if doc == None or doc.get("account", None) != acct_id:
        return _not_found("person", person_id)

    _person_body(doc, req["body"])
    store_collection("persons").update(person_id, doc)
    _signed_emit("person.updated", _person_public(doc))
    return respond(200, _person_public(doc))

# DELETE /v1/accounts/{id}/persons/{person_id} — delete a person (soft
# delete: kept for retrieval by id, hidden from lists).
def on_delete_person(req):
    err = _require_auth(req)
    if err != None:
        return err

    acct_id = req["params"]["id"]
    person_id = req["params"]["person_id"]
    doc = _person_get(person_id)
    if doc == None or doc.get("account", None) != acct_id:
        return _not_found("person", person_id)

    doc["_deleted"] = True
    store_collection("persons").update(person_id, doc)
    _signed_emit("person.deleted", _person_public(doc))
    return respond(200, {"id": person_id, "object": "person", "deleted": True})

# GET /v1/persons/{id} — shortcut retrieval by person id.
def on_retrieve_person_standalone(req):
    err = _require_auth(req)
    if err != None:
        return err

    doc = _person_get(req["params"]["id"])
    if doc == None:
        return _not_found("person", req["params"]["id"])
    return respond(200, _person_public(doc))

# POST /v1/persons/{id} — shortcut update by person id.
def on_update_person_standalone(req):
    err = _require_auth(req)
    if err != None:
        return err

    person_id = req["params"]["id"]
    doc = _person_get(person_id)
    if doc == None:
        return _not_found("person", person_id)

    _person_body(doc, req["body"])
    store_collection("persons").update(person_id, doc)
    _signed_emit("person.updated", _person_public(doc))
    return respond(200, _person_public(doc))
