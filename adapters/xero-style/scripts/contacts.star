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

    return _envelope("Contacts", contacts)

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
