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
    name = body.get("Name", "")
    if name == "":
        return _sf_error(400, "Required field missing: [Name]", "REQUIRED_FIELD_MISSING")

    record_id = _next_id(obj_type)
    doc = {}
    for k, v in body.items():
        doc[k] = v
    doc["Id"] = record_id
    doc["id"] = record_id

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

    # Strip internal id; return API-shaped record.
    result = {}
    for k, v in doc.items():
        if k != "id":
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

    body = _get_body(req)
    merged = {}
    for k, v in doc.items():
        merged[k] = v
    for k, v in body.items():
        merged[k] = v
    merged["Id"] = record_id
    merged["id"] = record_id
    col.update(record_id, merged)

    # Salesforce returns 204 No Content on successful PATCH.
    return respond(204)

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

    col.delete(record_id)
    return respond(204)
