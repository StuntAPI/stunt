# Files + File Links handlers — dispute/identity/document uploads
# (docs.stripe.com/api/files, docs.stripe.com/api/file_links).
#
# POST /v1/files is multipart/form-data (real Stripe: the upload goes to
# files.stripe.com with a purpose enum + the file part). stunt's engine hands
# handlers the raw multipart body via req.raw_body and the parse_multipart
# builtin splits it into parts {name, data, filename, content_type}. No
# binary retention: the file doc stores size/filename/title/type plus a
# SHA-256 content hash (crypto.sha256) in the internal field _sha256 — the
# real file object has no hash field, so the public renderer strips it.
#
# Purpose enum (the user-uploadable subset from
# docs.stripe.com/api/files/create): business_icon, business_logo,
# customer_signature, dispute_evidence, identity_document, pci_document,
# tax_document_user_upload.
#
# Real file shape: id file_*, object file, created, expires_at, filename,
# links (list object), purpose, size, title, type (extension: png/jpg/pdf/
# csv), url (files.stripe.com contents URL).
# Real file_link shape: id link_*, object file_link, created, expired,
# expires_at, file, livemode, metadata, url.
# Shared helpers (_require_auth, _next_id, _now, _not_found, _list_page,
# _newest_first, _get_query, _num) are in lib.star.

_CK_PURPOSES = [
    "business_icon",
    "business_logo",
    "customer_signature",
    "dispute_evidence",
    "identity_document",
    "pci_document",
    "tax_document_user_upload",
]

# _files_public renders the public file shape, injecting the live links list
# (every file_link pointing at this file, like the real expandable list).
def _files_public(doc):
    links = store_collection("file_links").list()
    mine = query_select(links, [["file", "=", doc["id"]]])
    out = {
        "id": doc["id"],
        "object": "file",
        "created": doc.get("created", 0),
        "expires_at": doc.get("expires_at", None),
        "filename": doc.get("filename", None),
        "links": {"object": "list", "data": [_links_public(l) for l in mine], "has_more": False, "url": "/v1/file_links?file=" + doc["id"]},
        "purpose": doc.get("purpose", None),
        "size": _num(doc.get("size", 0)),
        "title": doc.get("title", None),
        "type": doc.get("type", None),
        "url": "https://files.stripe.com/v1/files/" + doc["id"] + "/contents",
    }
    return out

# _links_public renders the public file_link shape with `expired` derived
# from the clock (a link whose expires_at has passed reads expired true).
def _links_public(doc):
    expires_at = doc.get("expires_at", None)
    expired = False
    if expires_at != None and _now() >= _num(expires_at):
        expired = True
    return {
        "id": doc["id"],
        "object": "file_link",
        "created": doc.get("created", 0),
        "expired": expired,
        "expires_at": expires_at,
        "file": doc.get("file", None),
        "livemode": False,
        "metadata": doc.get("metadata", {}),
        "url": "https://files.stripe.com/links/" + doc["id"],
    }

# _files_type maps a filename to the real Stripe file `type` (the extension
# without the dot, lowercased; None when the filename has none).
def _files_type(filename):
    if filename == None:
        return None
    dot = filename.rfind(".")
    if dot < 0 or dot + 1 >= len(filename):
        return None
    ext = filename[dot + 1:]
    out = ""
    for i in range(len(ext)):
        ch = ext[i]
        lch = ch
        if ch >= "A" and ch <= "Z":
            lch = chr(ord(ch) + 32)
        out = out + lch
    return out

# POST /v1/files — multipart upload. purpose (required, enum-validated) is a
# form field; the file part (any part carrying a filename) is the upload.
def on_create_file(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "files")
    if cached != None:
        return respond(cached["status"], _files_public(cached["doc"]))

    h = req.get("headers")
    ct = ""
    if h != None:
        ct = h.get("Content-Type", "")
        if ct == None:
            ct = ""
    if not ct.startswith("multipart/"):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "The file upload request must be multipart/form-data.", "param": "file"}})

    parts, perr = parse_multipart(ct, req["raw_body"])
    if perr != None:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Malformed multipart body.", "param": "file"}})

    purpose = None
    title = None
    data = None
    filename = None
    for i in range(len(parts)):
        p = parts[i]
        pname = p.get("name", None)
        if p.get("filename", None) != None:
            data = p.get("data", "")
            filename = p["filename"]
        elif pname == "purpose":
            purpose = p.get("data", None)
        elif pname == "title":
            title = p.get("data", None)
    if purpose == None or purpose == "":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: purpose.", "param": "purpose"}})

    ok = False
    for i in range(len(_CK_PURPOSES)):
        if _CK_PURPOSES[i] == purpose:
            ok = True
            break
    if not ok:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid purpose: " + purpose + ". Valid purposes are: business_icon, business_logo, customer_signature, dispute_evidence, identity_document, pci_document, tax_document_user_upload.", "param": "purpose"}})
    if data == None:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: file.", "param": "file"}})

    size = 0
    if data != None:
        size = len(data)
    doc = {
        "id": _next_id("file"),
        "object": "file",
        "created": _now(),
        "expires_at": None,
        "filename": filename,
        "purpose": purpose,
        "size": size,
        "title": title,
        "type": _files_type(filename),
        "_sha256": crypto.sha256(data),
    }
    store_collection("files").insert(doc)
    _idempotent_remember(req, "files", 201, doc["id"])
    return respond(201, _files_public(doc))

# GET /v1/files — list uploads (filter purpose; newest first).
def on_list_files(req):
    err = _require_auth(req)
    if err != None:
        return err

    docs = store_collection("files").list()
    purpose = _get_query(req, "purpose")
    if purpose != "":
        docs = query_select(docs, [["purpose", "=", purpose]])
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "file")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_files_public(d) for d in page], "has_more": has_more, "url": "/v1/files"})

# GET /v1/files/{id} — retrieve an upload.
def on_retrieve_file(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("files").get(id)
    if doc == None:
        return _not_found("file", id)
    return respond(200, _files_public(doc))

# POST /v1/file_links — create a public link to a file (file required and
# must exist; expires_at None = never expires).
def on_create_file_link(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    fid = body.get("file", None)
    if fid == None or fid == "":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: file.", "param": "file"}})
    if store_collection("files").get(fid) == None:
        return respond(400, {"error": {"code": "resource_missing", "message": "No such file: '" + fid + "'", "param": "file", "type": "invalid_request_error"}})

    expires_at = body.get("expires_at", None)
    if expires_at != None:
        expires_at = _num(expires_at)
        if expires_at <= 0:
            expires_at = None

    doc = {
        "id": _next_id("link"),
        "object": "file_link",
        "created": _now(),
        "expires_at": expires_at,
        "file": fid,
        "metadata": body.get("metadata", {}),
    }
    store_collection("file_links").insert(doc)
    return respond(201, _links_public(doc))

# GET /v1/file_links — list links (filter file; newest first).
def on_list_file_links(req):
    err = _require_auth(req)
    if err != None:
        return err

    docs = store_collection("file_links").list()
    fid = _get_query(req, "file")
    if fid != "":
        docs = query_select(docs, [["file", "=", fid]])
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "file_link")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_links_public(d) for d in page], "has_more": has_more, "url": "/v1/file_links"})

# GET /v1/file_links/{id} — retrieve a link.
def on_retrieve_file_link(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("file_links").get(id)
    if doc == None:
        return _not_found("file_link", id)
    return respond(200, _links_public(doc))

# POST /v1/file_links/{id} — update a link (expires_at, metadata).
def on_update_file_link(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("file_links")
    doc = c.get(id)
    if doc == None:
        return _not_found("file_link", id)

    body = req["body"]
    if body == None:
        body = {}
    if body.get("expires_at", None) != None:
        exp = _num(body.get("expires_at"))
        if exp <= 0:
            exp = None
        doc["expires_at"] = exp
    meta = body.get("metadata", None)
    if meta != None and type(meta) == "dict":
        doc["metadata"] = meta

    c.update(id, doc)
    return respond(200, _links_public(doc))
