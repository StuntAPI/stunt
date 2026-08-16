# Contacts handlers — list, create/update, single get, archive.
#
# Requires Bearer + xero-tenant-id.
# STATEFUL contacts stored in the "contacts" collection.
#
# GET /api.xro/2.0/Contacts       → { Id, Status, Contacts: [...] }
# PUT /api.xro/2.0/Contacts       → { Id, Status, Contacts: [...] } (create/update)
# GET /api.xro/2.0/Contacts/{id}  → { Id, Status, Contacts: [...] }
# PUT /api.xro/2.0/Contacts/{id}  → { Id, Status, Contacts: [...] } (update/archive:
#                                   ContactStatus "ARCHIVED" is Xero's soft delete)

# on_list_contacts lists all contacts.
def on_list_contacts(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    c = store_collection("contacts")
    docs = c.list()

    contacts = []
    for doc in docs:
        contacts.append(_contact_public(doc))

    contacts = _apply_contact_filters(req, contacts)
    contacts, next_page = _list_page(req, contacts)
    return _envelope("Contacts", contacts, next_page)

# _apply_contact_filters maps the real Xero GET /Contacts query params to
# filters, applied before paging like the real API: `search` (case-blind
# partial match on Name or EmailAddress) plus the shared `where`/`order`
# params.
def _apply_contact_filters(req, contacts):
    search = _trim(_get_query(req, "search"))
    if search != "":
        low = _lower(search)
        out = []
        for ct in contacts:
            name = _lower(str(ct.get("Name", "")))
            email = _lower(str(ct.get("EmailAddress", "")))
            if _contains(name, low) or _contains(email, low):
                out.append(ct)
        contacts = out
    return _apply_list_filters(req, contacts)

# on_put_contacts creates or updates contacts.
def on_put_contacts(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    contacts_in = body.get("Contacts")
    if contacts_in == None:
        # Single contact create.
        contacts_in = [body]

    result = []
    c = store_collection("contacts")
    for ct_in in contacts_in:
        name = ct_in.get("Name", "New Contact")
        if name == None:
            name = "New Contact"

        contact_id = _contact_id()
        doc = {
            "ContactID": contact_id,
            "ContactStatus": "ACTIVE",
            "Name": name,
            "EmailAddress": ct_in.get("EmailAddress", ""),
            "IsSupplier": ct_in.get("IsSupplier", False),
            "IsCustomer": ct_in.get("IsCustomer", True),
        }
        c.insert(doc)
        result.append(_contact_public(doc))

    return _envelope("Contacts", result)

# on_get_contact returns a single contact by ContactID. Archived contacts
# remain retrievable (with ContactStatus "ARCHIVED"), like the real API.
def on_get_contact(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    contact_id = req["params"].get("id", "")
    if contact_id == None or contact_id == "":
        return _xero_err(400, "BadRequest", "ValidationError", "ContactID is required")

    c = store_collection("contacts")
    for doc in c.list():
        if doc.get("ContactID", "") == contact_id:
            return _envelope("Contacts", [_contact_public(doc)])

    return _xero_err(404, "NotFound", "NotFound", "The contact was not found")

# on_put_contact updates a single contact by ContactID — Xero's ARCHIVE is an
# update that sets ContactStatus to "ARCHIVED" (the contact is never
# destroyed; it stays readable and reactivatable by setting "ACTIVE"). The
# body is a full or partial contact object; ContactStatus must be one of
# Xero's documented values.
_CONTACT_STATUSES = ["ACTIVE", "ARCHIVED"]

def on_put_contact(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    contact_id = req["params"].get("id", "")
    if contact_id == None or contact_id == "":
        return _xero_err(400, "BadRequest", "ValidationError", "ContactID is required")

    body = req.get("body")
    if body == None:
        body = {}
    if body.get("Contacts") != None:
        # A collection body addresses the single contact via the path id.
        items = body["Contacts"]
        if len(items) == 0:
            return _xero_err(400, "BadRequest", "ValidationError", "Contacts array is empty")
        body = items[0]

    status = body.get("ContactStatus", "")
    if status != None and status != "":
        ok = False
        for s in _CONTACT_STATUSES:
            if s == status:
                ok = True
        if not ok:
            return _xero_err(400, "BadRequest", "ValidationError", "ContactStatus must be ACTIVE or ARCHIVED")

    c = store_collection("contacts")
    for doc in c.list():
        if doc.get("ContactID", "") == contact_id:
            for k in body:
                if k != "ContactID":
                    doc[k] = body[k]
            c.update(doc.get("id", contact_id), doc)
            return _envelope("Contacts", [_contact_public(doc)])

    return _xero_err(404, "NotFound", "NotFound", "The contact was not found")
