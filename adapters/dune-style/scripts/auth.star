# Auth handler — Dune Analytics API.
#
# GET /api/v1/auth/validate → {valid: true, ...}

# Shared helpers (_bearer, _require_auth) are preloaded.

def on_validate(req):
    if not _require_auth(req):
        return respond(401, {"error": "Invalid API key"})

    return respond(200, {
        "valid": True,
        "plan": "plus",
        "execution_credits_per_month": 2500,
    })
