# Auth handler — auth.test.
#
# POST /api/auth.test
#   -> { ok:true, url, team, user, team_id, user_id, bot_id }

# Shared helpers (_require_auth, _ok, _err, TEAM_ID, etc.) are preloaded
# from scripts/lib.star.

def on_auth_test(req):
    err = _require_auth(req)
    if err != None:
        return err

    return _ok({
        "url": "https://stunt-test.slack.local/",
        "team": TEAM_NAME,
        "user": USER_NAME,
        "team_id": TEAM_ID,
        "user_id": USER_ID,
        "bot_id": BOT_ID,
        "is_enterprise_install": False,
    })
