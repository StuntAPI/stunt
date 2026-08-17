# SObjects handlers — describe global, describe object, and CRUD.
#
# GET    /services/data/v60.0/sobjects          -> describe global
# GET    /services/data/v60.0/sobjects/Account  -> describe object
# POST   /services/data/v60.0/sobjects/Account  -> create
# GET    /services/data/v60.0/sobjects/Account/{id} -> retrieve
# PATCH  /services/data/v60.0/sobjects/Account/{id} -> update
# DELETE /services/data/v60.0/sobjects/Account/{id} -> delete

# Shared helpers from lib.star.

# Object describe metadata (key fields only — synthetic).
_DESCRIBES = {
    "Account": {
        "name": "Account",
        "keyPrefix": "001",
        "label": "Account",
        "pluralLabel": "Accounts",
    },
    "Contact": {
        "name": "Contact",
        "keyPrefix": "003",
        "label": "Contact",
        "pluralLabel": "Contacts",
    },
    "Opportunity": {
        "name": "Opportunity",
        "keyPrefix": "006",
        "label": "Opportunity",
        "pluralLabel": "Opportunities",
    },
    "Lead": {
        "name": "Lead",
        "keyPrefix": "00Q",
        "label": "Lead",
        "pluralLabel": "Leads",
    },
    "User": {
        "name": "User",
        "keyPrefix": "005",
        "label": "User",
        "pluralLabel": "Users",
    },
}

# Fields for the describe endpoint.
_FIELDS = {
    "Account": ["Id", "Name", "Type", "Industry", "Phone", "Website", "BillingCity", "BillingState", "AnnualRevenue", "NumberOfEmployees"],
    "Contact": ["Id", "Name", "FirstName", "LastName", "Email", "Phone", "MailingCity", "MailingState"],
    "Opportunity": ["Id", "Name", "StageName", "Amount", "CloseDate", "Type", "Probability", "AccountId"],
    "Lead": ["Id", "Name", "FirstName", "LastName", "Company", "Status", "Email", "Phone"],
    "User": ["Id", "Name", "FirstName", "LastName", "Email", "Username", "IsActive"],
}

def on_describe_global(req):
    _, err = _require_token(req)
    if err != None:
        return err

    sobjects = []
    for name, desc in _DESCRIBES.items():
        sobjects.append({
            "name": desc["name"],
            "keyPrefix": desc["keyPrefix"],
            "label": desc["label"],
            "pluralLabel": desc["pluralLabel"],
            "labelPlural": desc["pluralLabel"],
            "activateable": False,
            "createable": True,
            "deletable": True,
            "queryable": True,
            "retrieveable": True,
            "searchable": True,
            "updateable": True,
            "urls": {
                "describe": "/services/data/v60.0/sobjects/" + name + "/describe",
                "sobject": "/services/data/v60.0/sobjects/" + name,
            },
        })

    return respond(200, {
        "encoding": "UTF-8",
        "maxBatchSize": 200,
        "sobjects": sobjects,
    })

def on_describe_object(req):
    _, err = _require_token(req)
    if err != None:
        return err

    obj_type = _obj_type_from_path(req)
    desc = _DESCRIBES.get(obj_type)
    if desc == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    fields_list = _FIELDS.get(obj_type, [])
    fields = []
    for f in fields_list:
        fields.append({
            "name": f,
            "label": f,
            "type": "string",
            "length": 255,
            "nillable": True,
            "createable": f != "Id",
            "updateable": f != "Id",
            "defaultedOnCreate": f == "Id",
        })

    return respond(200, {
        "name": desc["name"],
        "keyPrefix": desc["keyPrefix"],
        "label": desc["label"],
        "pluralLabel": desc["pluralLabel"],
        "labelPlural": desc["pluralLabel"],
        "createable": True,
        "deletable": True,
        "queryable": True,
        "retrieveable": True,
        "updateable": True,
        "fields": fields,
        "urls": {
            "describe": "/services/data/v60.0/sobjects/" + obj_type + "/describe",
            "sobject": "/services/data/v60.0/sobjects/" + obj_type,
        },
    })

# --- CRUD ---

def on_create(req):
    _, err = _require_token(req)
    if err != None:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    body = _get_body(req)
    name = body.get("Name") or ""
    if name == "":
        return _sf_error(400, "Required field missing: [Name]", "REQUIRED_FIELD_MISSING")

    record_id = _next_id(obj_type)
    doc = {}
    for k, v in body.items():
        doc[k] = v
    doc["Id"] = record_id
    doc["id"] = record_id
    # Salesforce stamps system fields server-side; they are live clock
    # values (RFC 3339 UTC), not client-settable.
    now = _now()
    doc["CreatedDate"] = now
    doc["LastModifiedDate"] = now
    doc["IsDeleted"] = False

    col.insert(doc)

    return respond(201, {
        "id": record_id,
        "success": True,
        "errors": [],
        "warnings": [],
    })

def on_retrieve(req):
    _, err = _require_token(req)
    if err != None:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    record_id = req["params"].get("id", "")
    if record_id == "":
        return _sf_error(400, "Missing record Id", "INVALID_FIELD")

    doc = col.get(record_id)
    if doc == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    # Deleted records live in the recycle bin: retrieval 404s like a missing
    # record (queryAll is the surface that still sees them).
    if doc.get("IsDeleted", False) == True:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    # Strip internal id + underscore-prefixed keys; return API-shaped record.
    result = {}
    for k, v in doc.items():
        if k != "id" and not k.startswith("_"):
            result[k] = v
    return respond(200, result)

def on_update(req):
    _, err = _require_token(req)
    if err != None:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    record_id = req["params"].get("id", "")
    doc = col.get(record_id)
    if doc == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    # Deleted records cannot be mutated — Salesforce's ENTITY_IS_DELETED.
    if doc.get("IsDeleted", False) == True:
        return _sf_error(404, "entity is deleted", "ENTITY_IS_DELETED")

    body = _get_body(req)
    merged = {}
    for k, v in doc.items():
        merged[k] = v
    for k, v in body.items():
        merged[k] = v
    merged["Id"] = record_id
    merged["id"] = record_id
    merged["LastModifiedDate"] = _now()
    col.update(record_id, merged)

    # Salesforce returns 204 No Content on successful PATCH.
    return respond(204)

# on_delete soft-deletes the record into the recycle bin, like the real API:
# the row is kept and flagged IsDeleted (plus a DeletedDate stamp), plain
# retrieval/PATCH/SOQL-query treat it as gone, and queryAll still returns it
# (with IsDeleted true) until it ages out of the bin.
def on_delete(req):
    _, err = _require_token(req)
    if err != None:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    record_id = req["params"].get("id", "")
    doc = col.get(record_id)
    if doc == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    if doc.get("IsDeleted", False) == True:
        # Already in the recycle bin; Salesforce reports the same
        # ENTITY_IS_DELETED error for repeated deletes.
        return _sf_error(404, "entity is deleted", "ENTITY_IS_DELETED")

    doc["IsDeleted"] = True
    doc["DeletedDate"] = _now()
    col.update(record_id, doc)
    return respond(204)

# on_upsert implements external-ID upsert:
#   PATCH /sobjects/{type}/{extIdField}/{extIdValue}
# No match -> insert (the external ID field is stamped from the URL); exactly
# one match -> update; multiple matches -> 300 Multiple Choices carrying the
# matching records, exactly as the real API resolves the ambiguity. Both the
# insert and update paths return 201 with {id, success, errors, created}.
def on_upsert(req):
    _, err = _require_token(req)
    if err != None:
        return err

    obj_type = _obj_type_from_path(req)
    col = _collection(obj_type)
    if col == None:
        return _sf_error(404, "The requested resource does not exist", "NOT_FOUND")

    params = req["params"]
    ext_field = params.get("extIdField", "")
    ext_value = params.get("extIdValue", "")
    if ext_field == "" or ext_value == "":
        return _sf_error(400, "Missing external ID field or value", "INVALID_FIELD")

    body = _get_body(req)

    # Recycle-bin rows are invisible to upsert matching, like /query.
    matches = []
    for d in col.list():
        if d.get("IsDeleted", False) == True:
            continue
        if _to_cmp_str(_soql_field(d, ext_field)) == ext_value:
            matches.append(d)

    if len(matches) > 1:
        recs = [_project(d, [], obj_type) for d in matches]
        return respond(300, recs)

    if len(matches) == 1:
        doc = matches[0]
        merged = {}
        for k, v in doc.items():
            merged[k] = v
        for k, v in body.items():
            if k != "attributes":
                merged[k] = v
        merged["Id"] = doc["Id"]
        merged["id"] = doc["Id"]
        merged["LastModifiedDate"] = _now()
        col.update(doc["Id"], merged)
        return respond(201, {
            "id": doc["Id"],
            "success": True,
            "errors": [],
            "created": False,
        })

    # No match -> insert. Same required-field rule as plain create.
    if (body.get("Name") or "") == "":
        return _sf_error(400, "Required field missing: [Name]", "REQUIRED_FIELD_MISSING")

    record_id = _next_id(obj_type)
    doc = {}
    for k, v in body.items():
        if k != "attributes":
            doc[k] = v
    doc[ext_field] = ext_value
    doc["Id"] = record_id
    doc["id"] = record_id
    now = _now()
    doc["CreatedDate"] = now
    doc["LastModifiedDate"] = now
    doc["IsDeleted"] = False
    col.insert(doc)

    return respond(201, {
        "id": record_id,
        "success": True,
        "errors": [],
        "created": True,
    })
