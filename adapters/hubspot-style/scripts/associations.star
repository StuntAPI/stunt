# Association handlers — the CRM associations pain point.
#
# PUT  /crm/v3/objects/contacts/{id}/associations/{toObjectType}/{toObjectId}/{associationType}
#   -> 200 (create association)
# GET  /crm/v3/objects/contacts/{id}/associations/{toObjectType}
#   -> {results:[{toObjectId, associationTypes:[{category, typeId}]}]}

# Shared helpers from lib.star.

def on_associate(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    params = req["params"]
    from_id = params.get("id", "")
    to_obj_type = params.get("toObjectType", "")
    to_obj_id = params.get("toObjectId", "")
    assoc_type = params.get("associationType", "")

    if from_id == "" or to_obj_type == "" or to_obj_id == "" or assoc_type == "":
        return _hs_error(400, "Missing required path parameters for association.", "VALIDATION")

    # Generate a unique association key.
    assoc_key = from_id + "_" + to_obj_type + "_" + to_obj_id + "_" + assoc_type

    doc = {
        "id": assoc_key,
        "fromObjectId": from_id,
        "toObjectType": to_obj_type,
        "toObjectId": to_obj_id,
        "associationType": assoc_type,
    }

    c = store_collection("associations")
    c.insert(doc)

    return respond(200, {"status": "complete"})

def on_list_associations(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    params = req["params"]
    from_id = params.get("id", "")
    to_obj_type = params.get("toObjectType", "")

    c = store_collection("associations")
    docs = c.list()

    results = []
    for d in docs:
        if d.get("fromObjectId") == from_id and d.get("toObjectType") == to_obj_type:
            results.append({
                "toObjectId": d.get("toObjectId", ""),
                "associationTypes": [{
                    "category": "HUBSPOT_DEFINED",
                    "typeId": 1,
                    "label": d.get("associationType", ""),
                }],
            })

    return respond(200, {"results": results})
