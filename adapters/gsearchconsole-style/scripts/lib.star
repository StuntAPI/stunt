# Shared library for gsearchconsole-style adapter scripts.

# _bearer extracts the token from "Authorization: Bearer <t>".
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer returns None if OK, or a 401 response if missing.
def _require_bearer(req):
    if _bearer(req) == "":
        return respond(401, {
            "error": {
                "code": 401,
                "message": "The request does not have valid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    return None

# _g_err returns a Google-style error response.
def _g_err(code, message, status):
    return respond(code, {
        "error": {
            "code": code,
            "message": message,
            "status": status,
        },
    })

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _to_int parses a decimal string to int.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _seed populates default sites.
def _seed():
    if store_kv_get("gsc", "seeded") == "yes":
        return
    store_kv_set("gsc", "seeded", "yes")

    sc = store_collection("sites")
    sc.insert({
        "id": "https://www.example.com/",
        "siteUrl": "https://www.example.com/",
        "permissionLevel": "siteFullUser",
    })
    sc.insert({
        "id": "https://blog.example.com/",
        "siteUrl": "https://blog.example.com/",
        "permissionLevel": "siteOwner",
    })
