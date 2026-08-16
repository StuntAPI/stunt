# Contacts handlers — list, create/update (upsert), single get, archive.
#
# Requires Bearer + xero-tenant-id.
# STATEFUL contacts stored in the "contacts" collection.
#
# GET /api.xro/2.0/Contacts       → { Id, Status, Contacts: [...] }
# PUT /api.xro/2.0/Contacts       → { Id, Status, Contacts: [...] } (upsert:
#                                   ContactID/ContactNumber match updates,
#                                   otherwise create; duplicate active Name
#                                   → 400 ValidationErrors)
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

# _find_contact resolves the stored contact an upsert addresses, matching by
# ContactID first, then ContactNumber (Xero's two upsert keys). Returns None
# when the input identifies no existing contact (i.e. a create).
def _find_contact(docs, ct_in):
    cid = ct_in.get("ContactID", "")
    if cid != None and cid != "":
        for d in docs:
            if str(d.get("ContactID", "")) == str(cid):
                return d
    num = ct_in.get("ContactNumber", "")
    if num != None and num != "":
        for d in docs:
            if str(d.get("ContactNumber", "")) == str(num):
                return d
    return None

# _name_taken reports whether an ACTIVE contact other than `self` already
# holds Name (Xero's rule: the contact name must be unique across all active
# contacts). self may be None (create path).
def _name_taken(docs, name, self):
    for d in docs:
        if self != None and d.get("ContactID", "") == self.get("ContactID", ""):
            continue
        if d.get("ContactStatus", "ACTIVE") != "ACTIVE":
            continue
        if str(d.get("Name", "")) == str(name):
            return True
    return False

# on_put_contacts creates or updates contacts (a true upsert, like the real
# API): a contact whose ContactID or ContactNumber matches an existing one
# UPDATES that record in place — the ContactID is stable and unspecified
# fields keep their stored values — while a contact identifying nothing
# existing is created. Creating (or renaming to) a Name already held by
# another active contact returns Xero's real validation error.
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
        # Single contact create/update.
        contacts_in = [body]

    result = []
    c = store_collection("contacts")
    docs = c.list()
    for ct_in in contacts_in:
        existing = _find_contact(docs, ct_in)
        if existing != None:
            name = ct_in.get("Name", existing.get("Name", ""))
            if name != None and _name_taken(docs, name, existing):
                return _validation_err("The contact name " + str(name) + " is already assigned to another contact. The contact name must be unique across all active contacts.")
            for k in ct_in:
                if k != "ContactID" and k != "id":
                    existing[k] = ct_in[k]
            c.update(existing.get("id", existing.get("ContactID", "")), existing)
            result.append(_contact_public(existing))
            continue

        name = ct_in.get("Name", "New Contact")
        if name == None:
            name = "New Contact"
        if _name_taken(docs, name, None):
            return _validation_err("The contact name " + str(name) + " is already assigned to another contact. The contact name must be unique across all active contacts.")

        doc = {
            "ContactID": _contact_id(),
            "ContactNumber": ct_in.get("ContactNumber", ""),
            "ContactStatus": "ACTIVE",
            "Name": name,
            "EmailAddress": ct_in.get("EmailAddress", ""),
            "IsSupplier": ct_in.get("IsSupplier", False),
            "IsCustomer": ct_in.get("IsCustomer", True),
        }
        c.insert(doc)
        docs.append(doc)
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
    docs = c.list()
    for doc in docs:
        if doc.get("ContactID", "") == contact_id:
            name = body.get("Name", doc.get("Name", ""))
            if name != None and _name_taken(docs, name, doc):
                return _validation_err("The contact name " + str(name) + " is already assigned to another contact. The contact name must be unique across all active contacts.")
            for k in body:
                if k != "ContactID" and k != "id":
                    doc[k] = body[k]
            c.update(doc.get("id", contact_id), doc)
            return _envelope("Contacts", [_contact_public(doc)])

    return _xero_err(404, "NotFound", "NotFound", "The contact was not found")
