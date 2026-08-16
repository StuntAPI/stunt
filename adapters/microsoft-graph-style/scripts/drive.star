# Microsoft Graph v1.0 — OneDrive metadata handlers.
#
# GET  /v1.0/me/drive                        → default drive info (incl. quota)
# GET  /v1.0/me/drive/root/children         → root folder children
# POST /v1.0/me/drive/root/children         → createFolder under root
# GET  /v1.0/me/drive/items/{id}/children   → children of a folder
# POST /v1.0/me/drive/items/{id}/children   → createFolder inside a folder
# GET  /v1.0/me/drive/items/{id}            → get a driveItem
# DELETE /v1.0/me/drive/items/{id}          → recycle bin (folder: cascades)
# POST /v1.0/me/drive/items/{id}/restore    → restore from the recycle bin
#
# Listing is PER-PARENT: every files doc carries a parentId ("root" for the
# drive root) and children endpoints filter by it. The upload plane lives in
# drive_upload.star; shared driveItem helpers live in lib.star.

# on_get_drive returns the default drive for the current user. The response
# always carries the quota object, so a ?select=quota (or $select=quota)
# query is satisfied by the same shape.
# GET /v1.0/me/drive (Bearer)
def on_get_drive(req):
    err = _require_bearer(req)
    if err != None:
        return err

    return respond(200, {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#drives/$entity",
        "id": "b!mock-drive-id-0001",
        "driveType": "business",
        "owner": {
            "user": {
                "displayName": "Alex Mockerman",
                "email": "alex@mock-tenant.onmicrosoft.com",
            },
        },
        "quota": {
            "total": 1099511627776,
            "used": 1073741824,
            "remaining": 1088438446080,
            "state": "normal",
        },
    })

# on_list_children returns the children of the drive root.
# GET /v1.0/me/drive/root/children (Bearer)
def on_list_children(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_files()
    entities = _children_entities("root")
    base_url = "https://graph.microsoft.com/v1.0/me/drive/root/children"
    return _apply_odata(entities, req["query"], base_url)

# on_list_item_children returns the children of a folder by id.
# GET /v1.0/me/drive/items/{id}/children (Bearer)
def on_list_item_children(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_files()
    parent_id = req["params"]["id"]
    fc = store_collection("files")
    if fc.get(parent_id) == None:
        return _err("itemNotFound", 404, "The resource could not be found.")

    entities = _children_entities(parent_id)
    base_url = "https://graph.microsoft.com/v1.0/me/drive/items/" + parent_id + "/children"
    return _apply_odata(entities, req["query"], base_url)

# on_create_child_root creates a folder under the drive root.
# POST /v1.0/me/drive/root/children (Bearer; {name, folder: {}})
def on_create_child_root(req):
    err = _require_bearer(req)
    if err != None:
        return err
    return _create_folder(req, "root")

# on_create_child_item creates a folder inside an existing folder.
# POST /v1.0/me/drive/items/{id}/children (Bearer; {name, folder: {}})
def on_create_child_item(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_files()
    parent_id = req["params"]["id"]
    fc = store_collection("files")
    parent = fc.get(parent_id)
    if parent == None or parent.get("folder") == None:
        return _err("itemNotFound", 404, "The parent folder could not be found.")
    return _create_folder(req, parent_id)

# on_get_item returns a single driveItem by id.
# GET /v1.0/me/drive/items/{id} (Bearer)
def on_get_item(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_files()
    item_id = req["params"].get("id", "")
    fc = store_collection("files")
    doc = fc.get(item_id)
    if doc == None:
        return _err("itemNotFound", 404, "The resource could not be found.")
    return respond(200, _drive_item(doc))

# on_delete_item moves a driveItem (file or folder) to the recycle bin, like
# real Graph: the row (plus, for a folder, EVERY descendant — cascade, so no
# child is left dangling under a missing parent) leaves the "files"
# collection and lands in the "recyclebin" collection. Stored content is
# kept so a restore brings the bytes back; the item 404s on every read path
# until restored.
# DELETE /v1.0/me/drive/items/{id} (Bearer) → 204 No Content
def on_delete_item(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_files()
    item_id = req["params"].get("id", "")
    fc = store_collection("files")
    doc = fc.get(item_id)
    if doc == None:
        return _err("itemNotFound", 404, "The resource could not be found.")

    subtree = _collect_subtree(fc, doc)
    rc = store_collection("recyclebin")
    now = clock.now_rfc3339()
    for d in subtree:
        d["_deleted_at"] = now
        d["_root_id"] = item_id
        rc.insert(d)
        fc.delete(d["id"])
    return respond(204)

# on_restore_item restores a driveItem from the recycle bin (real Graph's
# POST /drive/items/{item-id}/restore). The whole subtree that was cascaded
# into the bin with the item returns with its original ids and parent links,
# and the response is the restored root driveItem (201 Created). Restoring
# an item that is not in the bin (never deleted, or already restored) is the
# usual 404 itemNotFound.
# POST /v1.0/me/drive/items/{id}/restore (Bearer)
def on_restore_item(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed_files()
    item_id = req["params"].get("id", "")
    rc = store_collection("recyclebin")
    root = rc.get(item_id)
    if root == None:
        return _err("itemNotFound", 404, "The item is not in the recycle bin.")

    # Only the root of a delete batch restores its subtree; a cascaded
    # descendant (deleted WITH its parent) must come back via the parent.
    if root.get("_root_id", "") != item_id:
        return _err("invalidRequest", 409, "The item's parent folder is in the recycle bin; restore the parent folder instead.")

    fc = store_collection("files")
    for tomb in rc.list():
        if tomb.get("_root_id", "") != item_id:
            continue
        restored = {}
        for k, v in tomb.items():
            if not k.startswith("_"):
                restored[k] = v
        fc.insert(restored)
        rc.delete(tomb["id"])
    return respond(201, _drive_item(fc.get(item_id)))

# --- helpers ---

# _collect_subtree returns the doc plus every descendant (any depth), walking
# parentId links level by level. Pure read; callers move/delete the rows.
def _collect_subtree(fc, root_doc):
    out = [root_doc]
    frontier = [root_doc["id"]]
    i = 0
    while i < len(frontier):
        for d in fc.list():
            if d.get("parentId", "root") == frontier[i]:
                out.append(d)
                frontier.append(d["id"])
        i = i + 1
    return out

# _create_folder handles the createFolder body against a parent. Graph's
# default conflict behavior for createFolder is "fail" (409
# nameAlreadyExists); "rename" appends " (1)"-style suffixes.
def _create_folder(req, parent_id):
    body = req["body"]
    if body == None:
        body = {}
    name = body.get("name", "")
    if name == "" or body.get("folder") == None:
        return _err("invalidRequest", 400, "A folder item requires 'name' and a 'folder' facet.")

    conflict = body.get("@microsoft.graph.conflictBehavior", "fail")
    fc = store_collection("files")
    existing = _find_child_by_name(fc, parent_id, name)
    if existing != None:
        if conflict == "rename":
            name = _conflict_rename(fc, parent_id, name)
        elif conflict == "replace":
            return respond(200, _drive_item(existing))
        else:
            return _err("nameAlreadyExists", 409, "An item with the same name already exists under the parent.")

    doc = {
        "id": _next_item_id(),
        "name": name,
        "file": None,
        "folder": {"childCount": 0},
        "size": 0,
        "parentId": parent_id,
        "createdDateTime": "2024-06-15T12:00:00Z",
        "lastModifiedDateTime": "2024-06-15T12:00:00Z",
    }
    fc.insert(doc)
    return respond(201, _drive_item(doc))

# _children_entities lists the driveItems whose parentId matches.
def _children_entities(parent_id):
    fc = store_collection("files")
    entities = []
    for d in fc.list():
        if d.get("parentId", "root") == parent_id:
            entities.append(_drive_item(d))
    return entities

# _seed_files (the default OneDrive root children) moved to lib.star: the
# Excel workbook handlers resolve tables against drive items too.
