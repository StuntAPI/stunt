# Microsoft Graph v1.0 — OneDrive metadata handlers.
#
# GET  /v1.0/me/drive                        → default drive info (incl. quota)
# GET  /v1.0/me/drive/root/children         → root folder children
# POST /v1.0/me/drive/root/children         → createFolder under root
# GET  /v1.0/me/drive/items/{id}/children   → children of a folder
# POST /v1.0/me/drive/items/{id}/children   → createFolder inside a folder
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

# --- helpers ---

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

def _seed_files():
    fc = store_collection("files")
    docs = fc.list()
    if len(docs) > 0:
        return
    seed_files = [
        {
            "id": "file-000001-doc",
            "name": "Project Plan.docx",
            "file": {"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
            "folder": None,
            "size": 24576,
            "parentId": "root",
            "createdDateTime": "2024-03-01T10:00:00Z",
            "lastModifiedDateTime": "2024-06-10T15:30:00Z",
        },
        {
            "id": "file-000002-xls",
            "name": "Budget.xlsx",
            "file": {"mimeType": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
            "folder": None,
            "size": 53248,
            "parentId": "root",
            "createdDateTime": "2024-02-15T09:00:00Z",
            "lastModifiedDateTime": "2024-06-12T11:00:00Z",
        },
        {
            "id": "folder-000001-reports",
            "name": "Reports",
            "file": None,
            "folder": {"childCount": 5},
            "size": 0,
            "parentId": "root",
            "createdDateTime": "2024-01-20T08:00:00Z",
            "lastModifiedDateTime": "2024-06-14T16:00:00Z",
        },
    ]
    for f in seed_files:
        fc.insert(f)
