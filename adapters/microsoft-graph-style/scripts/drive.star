# Microsoft Graph v1.0 — OneDrive handlers.
#
# GET /v1.0/me/drive               → default drive info
# GET /v1.0/me/drive/root/children → root folder children (files/folders)

# on_get_drive returns the default drive for the current user.
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
    fc = store_collection("files")
    docs = fc.list()
    entities = []
    for d in docs:
        entities.append(_file_entity(d))

    base_url = "https://graph.microsoft.com/v1.0/me/drive/root/children"
    return _apply_odata(entities, req["query"], base_url)

# --- helpers ---

def _file_entity(doc):
    return {
        "id": doc["id"],
        "name": doc["name"],
        "file": doc.get("file", None),
        "folder": doc.get("folder", None),
        "size": doc.get("size", 0),
        "createdDateTime": doc.get("createdDateTime", "2024-01-01T00:00:00Z"),
        "lastModifiedDateTime": doc.get("lastModifiedDateTime", "2024-01-01T00:00:00Z"),
    }

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
            "createdDateTime": "2024-03-01T10:00:00Z",
            "lastModifiedDateTime": "2024-06-10T15:30:00Z",
        },
        {
            "id": "file-000002-xls",
            "name": "Budget.xlsx",
            "file": {"mimeType": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
            "folder": None,
            "size": 53248,
            "createdDateTime": "2024-02-15T09:00:00Z",
            "lastModifiedDateTime": "2024-06-12T11:00:00Z",
        },
        {
            "id": "folder-000001-reports",
            "name": "Reports",
            "file": None,
            "folder": {"childCount": 5},
            "size": 0,
            "createdDateTime": "2024-01-20T08:00:00Z",
            "lastModifiedDateTime": "2024-06-14T16:00:00Z",
        },
    ]
    for f in seed_files:
        fc.insert(f)
