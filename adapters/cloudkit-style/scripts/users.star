# Users handler — CloudKit Web Services user endpoint.
#
# GET .../users/current → current user info

# on_current_user returns the current user record.
def on_current_user(req):
    auth, err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "userRecordName": "_owner",
        "firstName": "Test",
        "lastName": "User",
    })
