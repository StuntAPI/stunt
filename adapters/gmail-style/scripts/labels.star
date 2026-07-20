# Label handlers — list, create.
#
# GET   /gmail/v1/users/{userId}/labels    → {labels:[{id, name, type, color}]}
# POST  /gmail/v1/users/{userId}/labels    → create label → {id, name, ...}
#
# Shared helpers are preloaded from scripts/lib.star.

# on_list_labels returns all labels (system + user).
def on_list_labels(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    lc = store_collection("labels")
    labels = []
    for doc in lc.list():
        labels.append({
            "id": doc["id"],
            "name": doc["name"],
            "type": doc["type"],
            "color": doc.get("color", None),
        })

    return respond(200, {
        "labels": labels,
    })

# on_create_label creates a user label.
def on_create_label(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    name = body.get("name", "")
    if name == "" or name == None:
        return _bad_request("Label name is required")

    # Check for duplicates.
    lc = store_collection("labels")
    for existing in lc.list():
        if existing.get("name") == name:
            return respond(200, {
                "id": existing["id"],
                "name": existing["name"],
                "type": existing.get("type", "user"),
                "color": existing.get("color", None),
            })

    seq = _seq("label_seq")
    label_id = "user-" + str(seq + 1)

    doc = {
        "id": label_id,
        "name": name,
        "type": "user",
        "color": body.get("color", None),
    }

    lc.insert(doc)

    return respond(200, {
        "id": label_id,
        "name": name,
        "type": "user",
        "color": doc.get("color", None),
    })
