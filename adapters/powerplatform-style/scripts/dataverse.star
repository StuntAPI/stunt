# Dataverse handler — Microsoft Power Platform Dataverse entities (accounts).
#
# GET    .../accounts              → OData {value:[...]} ($select, $count, paged)
# GET    .../accounts({accountid}) → single account
# POST   .../accounts              → create (201 + Location)
# PATCH  .../accounts({accountid}) → update (204)
# DELETE .../accounts({accountid}) → delete (204)

# _accounts returns the accounts collection, seeded once from _ACCOUNTS. The
# accountid is the storage key, so CRUD targets it directly.
def _accounts():
    c = store_collection("dataverse_accounts")
    if store_kv_get("pp", "accounts_seeded") == None:
        for a in _ACCOUNTS:
            seed = {"id": a["accountid"]}
            for k in a:
                seed[k] = a[k]
            c.insert(seed)
        store_kv_set("pp", "accounts_seeded", "1")
    return c

# _select_fields projects each doc to the OData $select comma-separated fields.
def _select_fields(docs, sel):
    if sel == None or sel == "":
        return docs
    fields = [f.strip() for f in sel.split(",")]
    out = []
    for d in docs:
        proj = {}
        for f in fields:
            if f in d:
                proj[f] = d[f]
        out.append(proj)
    return out

def on_list_accounts(req):
    err = _require_bearer(req)
    if err != None:
        return err

    docs = _accounts().list()
    base_path = "/v2/environments/" + req["params"]["env"] + "/api/data/v9.2/accounts"
    total = len(docs)
    page, next_link = _list_page(req, docs, base_path)

    q = req.get("query")
    sel = ""
    if q != None:
        sel = q.get("$select", "")

    resp = {
        "@odata.context": "https://example.api.crm.dynamics.com/api/data/v9.2/$metadata#accounts",
        "value": _select_fields(page, sel),
    }
    if q != None and q.get("$count") == "true":
        resp["@odata.count"] = total
    if next_link != None:
        resp["@odata.nextLink"] = next_link
    return respond(200, resp)

# GET .../accounts({accountid})
def on_retrieve_account(req):
    err = _require_bearer(req)
    if err != None:
        return err

    id = req["params"]["accountid"]
    a = _accounts().get(id)
    if a == None:
        return respond(404, {"error": {"code": "0x80040217", "message": "account With Id = " + id + " Does Not Exist"}})
    return respond(200, a)

# POST .../accounts
def on_create_account(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}
    if body.get("accountid", "") == "":
        body["accountid"] = "acc-" + str(store_kv_incr("pp", "account_seq"))
    body["id"] = body["accountid"]
    _accounts().insert(body)
    return respond(201, body, {
        "Location": "/v2/environments/" + req["params"]["env"] + "/api/data/v9.2/accounts(" + body["accountid"] + ")",
        "OData-Version": "4.0",
    })

# PATCH .../accounts({accountid})
def on_update_account(req):
    err = _require_bearer(req)
    if err != None:
        return err

    id = req["params"]["accountid"]
    c = _accounts()
    a = c.get(id)
    if a == None:
        return respond(404, {"error": {"code": "0x80040217", "message": "account With Id = " + id + " Does Not Exist"}})

    body = req["body"]
    if body != None:
        for k in body:
            a[k] = body[k]
        c.update(id, a)
    return respond(204, None)

# DELETE .../accounts({accountid})
def on_delete_account(req):
    err = _require_bearer(req)
    if err != None:
        return err

    id = req["params"]["accountid"]
    c = _accounts()
    if c.get(id) == None:
        return respond(404, {"error": {"code": "0x80040217", "message": "account With Id = " + id + " Does Not Exist"}})
    c.delete(id)
    return respond(204, None)
