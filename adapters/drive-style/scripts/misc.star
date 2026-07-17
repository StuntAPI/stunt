# Misc handlers — about (quota) and changes endpoints.
# These are synthetic and require no state.

# GET /drive/v3/about — return synthetic storage quota + user.
def on_about(req):
    return respond(200, {
        "storageQuota": {
            "limit": "16106127360",
            "usage": "8496111488",
            "usageInDrive": "3245064790",
            "usageInDriveTrash": "3429731",
        },
        "user": {
            "displayName": "Local Test User",
            "emailAddress": "test-user@example.local",
            "permissionId": "synthetic-permission-id",
            "kind": "drive#user",
        },
        "kind": "drive#about",
    })

# GET /drive/v3/changes — return a minimal synthetic, empty change list.
def on_changes(req):
    return respond(200, {
        "changes": [],
        "newStartPageToken": "synthetic-page-token-1",
        "kind": "drive#changeList",
    })
