# Shared library for apps-script-style adapter scripts.

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

# _gen_script_id generates a realistic Apps Script project ID.
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

def _gen_script_id(seq):
    base = ""
    val = seq * 7919 + 104729
    for i in range(48):
        base = base + _B64URL[val % 64]
        val = val // 64 + 31
    return base[:48]

# _seed creates a default project with content.
def _seed():
    if store_kv_get("apps-script", "seeded") == "yes":
        return
    store_kv_set("apps-script", "seeded", "yes")

    script_id = _gen_script_id(0)
    store_kv_set("apps-script", "default_script_id", script_id)

    pc = store_collection("projects")
    pc.insert({
        "id": script_id,
        "scriptId": script_id,
        "title": "Untitled project",
        "parentId": None,
        "createTime": "2024-01-01T00:00:00.000Z",
        "updateTime": "2024-01-01T00:00:00.000Z",
        "content": {
            "files": [
                {
                    "name": "Code",
                    "type": "SERVER_JS",
                    "source": "function helloWorld() {\n  Logger.log('Hello, World!');\n}\n",
                },
            ],
        },
    })

# _find_project returns the project by scriptId, or None.
def _find_project(script_id):
    pc = store_collection("projects")
    for p in pc.list():
        if p.get("scriptId") == script_id or p.get("id") == script_id:
            return p
    return None
