# Contacts handlers — list and create/update.
#
# Requires Bearer + xero-tenant-id.
# STATEFUL contacts stored in the "contacts" collection.
#
# GET /api.xro/2.0/Contacts  → { Id, Status, Contacts: [...] }
# PUT /api.xro/2.0/Contacts  → { Id, Status, Contacts: [...] } (create/update)

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
