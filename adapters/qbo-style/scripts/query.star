# Query handler — SQL-like query endpoint.
#
# GET/POST /v3/company/{realmId}/query?query=SELECT * FROM Customer
# -> { QueryResponse: { Customer: [...] / Invoice: [...] }, time }
#
# We pattern-match the entity name (Customer/Invoice/etc) and return the
# appropriate collection. No real SQL parsing.

# Shared helpers (_bearer, _require_token, _realm_matches, _detect_entity,
# _get_query, _fault, _now) from lib.star.

def on_query(req):
    token_doc, err = _require_token(req)
    if err != None:
        return err

    realm_id = req["params"]["realmId"]
    if not _realm_matches(token_doc, realm_id):
        return _auth_fault()

    query_str = _get_query(req)
    entity = _detect_entity(query_str)
    if entity == "":
        return _fault(400, "400", "Invalid query", "Could not determine entity from query: " + query_str)

    if entity == "Customer":
        c = store_collection("customers")
        docs = c.list()
        return respond(200, {"QueryResponse": {"Customer": docs, "maxResults": len(docs)}, "time": _now()})

    if entity == "Invoice":
        c = store_collection("invoices")
        docs = c.list()
        return respond(200, {"QueryResponse": {"Invoice": docs, "maxResults": len(docs)}, "time": _now()})

    # Unsupported entity → empty response.
    return respond(200, {"QueryResponse": {"maxResults": 0}, "time": _now()})
