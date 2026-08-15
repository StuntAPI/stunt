# Misc handlers — about (quota) and the changes feed.
#
# The changes feed is REAL: every file mutation (upload, metadata create,
# patch, delete) appends an entry to the changes collection (see
# _record_change in lib.star); GET /drive/v3/changes replays them by
# pageToken cursor.

# GET /drive/v3/about — return synthetic storage quota + user.
# Quota numbers are computed at runtime (never long digit literals).
def on_about(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    gib = 1024 * 1024 * 1024
    mib = 1024 * 1024
    return respond(200, {
        "storageQuota": {
            "limit": str(15 * gib),
            "usage": str(7 * gib + 350 * mib),
            "usageInDrive": str(3 * gib + 45 * mib),
            "usageInDriveTrash": str(3 * mib + 731 * 1024),
        },
        "user": {
            "displayName": "Local Test User",
            "emailAddress": "test-user@example.local",
            "permissionId": "synthetic-permission-id",
            "kind": "drive#user",
        },
        "kind": "drive#about",
    })

# GET /drive/v3/changes/startPageToken — the cursor a client passes to
# /drive/v3/changes to see only FUTURE changes (the current change-token
# sequence value).
def on_changes_start(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    seq = store_kv_get("drive", "change_seq")
    if seq == None:
        seq = "0"
    return respond(200, {
        "kind": "drive#startPageToken",
        "startPageToken": seq,
    })

# GET /drive/v3/changes — replay change entries with token > pageToken, in
# token order, honoring pageSize/pageToken paging. The last page carries
# newStartPageToken (the current sequence value) for the next polling
# round, like the real API.
def on_changes(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    q = _get_query(req)
    page_token = q.get("pageToken", "")
    if page_token == None or page_token == "":
        return _drive_err(400, "The 'pageToken' parameter is required", "INVALID_ARGUMENT")
    after = _to_int(page_token)

    c = store_collection("changes")
    entries = []
    for e in c.list():
        if _to_num(e.get("token", 0)) > after:
            entries.append(e)
    ordered = query_select(entries, None, "token", "asc", None, None, None)

    seq = store_kv_get("drive", "change_seq")
    if seq == None:
        seq = "0"

    page, next_token = _list_page(req, ordered)
    result = {
        "kind": "drive#changeList",
        "changes": [_change_token_view(e) for e in page],
    }
    if next_token != None:
        result["nextPageToken"] = next_token
    else:
        result["newStartPageToken"] = seq
    return respond(200, result)
