# Misc handlers — myself + serverInfo.
#
# GET /rest/api/3/myself -> {accountId, displayName, emailAddress, active}
# GET /rest/api/3/serverInfo -> {version, ...}

# Shared helpers from lib.star.

def on_myself(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "accountId": "5f1b3a4c5d6e7f8a9b0c1d2e",
        "displayName": "Alex Chen",
        "emailAddress": "alex-chen@user-fixture.example",
        "active": True,
        "timeZone": "UTC",
        "locale": "en_US",
        "accountType": "atlassian",
        "self": "https://mock-jira.atlassian.net/rest/api/3/myself",
    })

def on_server_info(req):
    _, err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "baseUrl": "https://mock-jira.atlassian.net",
        "version": "1001.0.0-SNAPSHOT",
        "versionNumbers": [1001, 0, 0],
        "deploymentType": "Cloud",
        "buildDate": "2024-01-01T00:00:00.000+0000",
        "buildNumber": 100001,
        "serverTime": _now(),
        "scmInfo": "mock-scm-info",
        "serverTitle": "Jira",
    })
