# Fixture lib — predeclared into every handler script, like the engine does.

def _require_bearer(req):
    if _bearer(req) == "":
        return respond(401, {"error": "unauthenticated"})
    return None

def _bearer(req):
    h = req["headers"]
    if h == None:
        return ""
    return h.get("authorization", "")

def _list_page(req, docs):
    limit = req.get("query").get("limit", 10)
    return paginate(docs, limit)
