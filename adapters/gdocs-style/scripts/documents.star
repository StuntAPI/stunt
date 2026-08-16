# Document handlers — Google Docs API endpoints.
#
# POST /v1/documents                           → create document
# GET  /v1/documents/{documentId}              → get document (structural model)
# POST /v1/documents/{documentId}/batchUpdate  → batch structural updates
# GET  /v1/documents/{documentId}/revisions    → list revisions
#
# batchUpdate understands the real request vocabulary: insertText,
# updateTextStyle, updateParagraphStyle, createParagraphBullets,
# deleteParagraphBullets, deleteContentRange, insertPageBreak and
# insertInlineImage. Unknown request types are 400 INVALID_ARGUMENT.
# The body may be JSON, or multipart/form-data with a "metadata" JSON part
# plus image parts consumed by insertInlineImage requests in order.

# on_create_document creates a new document.
def on_create_document(req):
    err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    title = body.get("title", "Untitled document")
    if title == None:
        title = "Untitled document"

    seq = store_kv_incr("gdocs", "doc_seq") + 1
    doc_id = _gen_doc_id(seq)

    doc = _build_doc(doc_id, title, [])
    _touch_doc(doc)
    dc = store_collection("documents")
    dc.insert(doc)

    return respond(200, _doc_response(doc))

# on_get_document returns a document with the structural content model.
def on_get_document(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    doc_id = req["params"]["documentId"]
    doc = _find_doc(doc_id)
    if doc == None:
        return _g_err(404, "The document " + doc_id + " does not exist.", "NOT_FOUND")

    return respond(200, _doc_response(doc))

# on_batch_update processes batch structural updates over the document's
# paragraph model. Requests are applied in order; each index is a 1-based
# UTF-16 code unit (an astral character counts as two).
def on_batch_update(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    doc_id = req["params"]["documentId"]
    doc = _find_doc(doc_id)
    if doc == None:
        return _g_err(404, "The document " + doc_id + " does not exist.", "NOT_FOUND")

    requests, image_parts, jerr = _parse_update_body(req)
    if jerr != None:
        return _bad_request(jerr)
    if requests == None:
        requests = []
    if type(requests) != "list":
        return _bad_request("Invalid requests: must be a list.")
    if len(requests) == 0:
        return _bad_request("Invalid requests: at least one request is required.")

    model = _doc_model(doc)
    inline_objects = doc.get("inlineObjects")
    if inline_objects == None:
        inline_objects = {}
    lists = doc.get("lists")
    if lists == None:
        lists = {}

    replies = []
    img_i = 0
    for i in range(len(requests)):
        r = requests[i]
        if r == None or type(r) != "dict":
            return _bad_request("Invalid requests[" + str(i) + "]: must be an object.")
        known = []
        for k in _KNOWN_REQUESTS:
            if k in r:
                known.append(k)
        if len(known) == 0:
            return _bad_request(
                "Invalid requests[" + str(i) + "]: unknown request type. One of "
                + ", ".join(_KNOWN_REQUESTS) + " must be set.")
        if len(known) > 1:
            return _bad_request(
                "Invalid requests[" + str(i) + "]: exactly one request type must be set, got "
                + str(len(known)) + ".")
        kind = known[0]
        spec = r.get(kind)
        if spec == None or type(spec) != "dict":
            return _bad_request("Invalid requests[" + str(i) + "]." + kind + ": must be an object.")

        if kind == "insertText":
            text = spec.get("text")
            if text == None or type(text) != "string":
                return _bad_request("Invalid requests[" + str(i) + "].insertText.text: required string.")
            index, ierr = _resolve_location(spec, model)
            if ierr != None:
                return _bad_request("Invalid requests[" + str(i) + "].insertText." + ierr)
            msg = _apply_insert_text(model, index, text)
            if msg != None:
                return _bad_request("Invalid requests[" + str(i) + "].insertText." + msg)
            replies.append({})

        elif kind == "deleteContentRange":
            rng = spec.get("range")
            if rng == None or type(rng) != "dict":
                return _bad_request("Invalid requests[" + str(i) + "].deleteContentRange.range: required.")
            start = _num_to_int(rng.get("startIndex"), -1)
            end = _num_to_int(rng.get("endIndex"), -1)
            msg = _apply_delete_range(model, start, end)
            if msg != None:
                return _bad_request("Invalid requests[" + str(i) + "].deleteContentRange." + msg)
            replies.append({})

        elif kind == "updateParagraphStyle":
            msg = _apply_update_paragraph_style(model, spec)
            if msg != None:
                return _bad_request("Invalid requests[" + str(i) + "].updateParagraphStyle." + msg)
            replies.append({})

        elif kind == "updateTextStyle":
            msg = _apply_update_text_style(model, spec)
            if msg != None:
                return _bad_request("Invalid requests[" + str(i) + "].updateTextStyle." + msg)
            replies.append({})

        elif kind == "createParagraphBullets":
            res = _apply_create_paragraph_bullets(model, spec)
            if res[0] != None:
                return _bad_request("Invalid requests[" + str(i) + "].createParagraphBullets." + res[0])
            lists[res[1][0]] = res[1][1]
            replies.append({})

        elif kind == "deleteParagraphBullets":
            msg = _apply_delete_paragraph_bullets(model, spec)
            if msg != None:
                return _bad_request("Invalid requests[" + str(i) + "].deleteParagraphBullets." + msg)
            replies.append({})

        elif kind == "insertPageBreak":
            index, ierr = _resolve_location(spec, model)
            if ierr != None:
                return _bad_request("Invalid requests[" + str(i) + "].insertPageBreak." + ierr)
            msg = _apply_insert_element(model, index, {"t": "brk"})
            if msg != None:
                return _bad_request("Invalid requests[" + str(i) + "].insertPageBreak." + msg)
            replies.append({})

        else:  # insertInlineImage
            reply, ierr = _apply_inline_image(model, spec, image_parts, img_i)
            if ierr != None:
                return _bad_request("Invalid requests[" + str(i) + "].insertInlineImage." + ierr)
            oid = reply[0]
            img_i = reply[1]
            raw = reply[2]
            mime = reply[3]
            content_uri = reply[4]
            size = spec.get("objectSize")
            if size == None or type(size) != "dict":
                size = {
                    "height": {"magnitude": 15, "unit": "PT"},
                    "width": {"magnitude": 15, "unit": "PT"},
                }
            inline_objects[oid] = {
                "objectId": oid,
                "inlineObjectProperties": {
                    "embeddedObject": {
                        "mimeType": mime,
                        "imageProperties": {"contentUri": content_uri},
                        "size": size,
                    },
                },
            }
            if raw != None:
                bs = store_blob("gdocs")
                bs.put(oid, raw, mime)
            replies.append({"objectId": oid})

    doc["model"] = model
    doc["inlineObjects"] = inline_objects
    doc["lists"] = lists
    _touch_doc(doc)
    dc = store_collection("documents")
    dc.update(doc.get("id"), doc)

    return respond(200, {
        "documentId": doc_id,
        "replies": replies,
    })

# on_get_revisions returns the revision history for a document.
def on_get_revisions(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    doc_id = req["params"]["documentId"]
    doc = _find_doc(doc_id)
    if doc == None:
        return _g_err(404, "The document " + doc_id + " does not exist.", "NOT_FOUND")

    revisions = doc.get("revisions")
    if revisions == None or len(revisions) == 0:
        revisions = [
            {
                "id": "1",
                "modifiedTime": _now_ms(),
                "lastModifier": {"displayName": "Test User", "me": True},
            },
        ]

    return respond(200, {
        "documentId": doc_id,
        "revisions": revisions,
    })

# --- helpers ---------------------------------------------------------------

_KNOWN_REQUESTS = [
    "insertText",
    "updateTextStyle",
    "updateParagraphStyle",
    "createParagraphBullets",
    "deleteParagraphBullets",
    "deleteContentRange",
    "insertPageBreak",
    "insertInlineImage",
]

# _parse_update_body extracts the requests list (and any raw image parts for
# insertInlineImage) from a JSON or multipart batchUpdate body. Returns
# [requests, image_parts, error].
def _parse_update_body(req):
    ct = req["headers"].get("Content-Type", "")
    if ct == None:
        ct = ""
    if ct.startswith("multipart/"):
        parts, perr = parse_multipart(ct, req["raw_body"])
        if perr != None:
            return [None, [], "malformed multipart body: " + perr]
        meta = None
        image_parts = []
        for p in parts:
            if p["filename"] == None and (p["name"] == "metadata" or p["name"] == "requests"):
                meta = p["data"]
            elif p["filename"] != None:
                image_parts.append(p)
        if meta == None and len(parts) == 1 and parts[0]["filename"] == None:
            meta = parts[0]["data"]
        if meta == None:
            return [None, [], "multipart body has no metadata part"]
        body = json_safe_decode(meta)
        if type(body) != "dict":
            return _g_err(400, "The metadata part must be a JSON object.", "INVALID_ARGUMENT")
        if body == None or type(body) != "dict":
            return [None, [], "metadata part is not a JSON object"]
        return [body.get("requests"), image_parts, None]

    body = req["body"]
    if body == None:
        body = {}
    return [body.get("requests"), [], None]

# _resolve_location reads location.index or endOfSegmentLocation from a
# request spec. endOfSegmentLocation maps to the index of the document's
# final newline (insert before it = append to the last paragraph).
def _resolve_location(spec, model):
    loc = spec.get("location")
    eos = spec.get("endOfSegmentLocation")
    if loc != None and type(loc) == "dict":
        return [_num_to_int(loc.get("index"), 0), None]
    if eos != None:
        return [_u16_len(_model_text(model)), None]
    return [0, "location: required (location or endOfSegmentLocation)."]

# _apply_inline_image resolves the image payload for one insertInlineImage
# request and inserts the inline-object element. Returns
# [[objectId, next_img_i, raw_or_None, mimeType, contentUri], error].
def _apply_inline_image(model, spec, image_parts, img_i):
    # Returns [[objectId, next_img_i, raw, mimeType, contentUri], error].
    index, ierr = _resolve_location(spec, model)
    if ierr != None:
        return [None, ierr]

    uri = spec.get("uri")
    if uri != None and type(uri) != "string":
        uri = None
    mime = ""
    data = None
    encoded = True  # data is base64 unless it came from a multipart file part
    img_data = spec.get("imageData")
    if img_data != None and type(img_data) == "dict":
        data = img_data.get("data")
        m = img_data.get("mimeType")
        if m != None and m != "":
            mime = m
    if data == None:
        d2 = spec.get("data")
        if d2 != None and type(d2) == "string":
            data = d2
            m = spec.get("mimeType")
            if m != None and m != "":
                mime = m
    if data == None and img_i < len(image_parts):
        part = image_parts[img_i]
        img_i = img_i + 1
        data = part["data"]
        encoded = False  # multipart parts carry raw bytes
        m = part["content_type"]
        if m != None and m != "":
            mime = m

    raw = None
    if data != None and data != "":
        if encoded:
            dec = _decode_b64(data)
            if dec[1] != None:
                return [None, "imageData: " + dec[1]]
            raw = dec[0]
        else:
            raw = data
    if raw == None and (uri == None or uri == ""):
        return [None, "uri or image data (base64 imageData or a multipart file part) is required."]

    oid = "kix." + _gen_token(store_kv_incr("gdocs", "obj_seq"))
    content_uri = ""
    if uri != None and uri != "":
        content_uri = uri
        if mime == "":
            mime = _guess_image_mime(uri)
    else:
        if mime == "":
            mime = "image/png"
        content_uri = "https://stunt-media.local/gdocs/" + oid

    msg = _apply_insert_element(model, index, {"t": "obj", "objectId": oid})
    if msg != None:
        return [None, msg]
    return [[oid, img_i, raw, mime, content_uri], None]

# _doc_response builds the API response shape for a document, deriving the
# ranged body.content from the internal model.
def _doc_response(doc):
    model = _doc_model(doc)
    out = {
        "documentId": doc.get("documentId", doc.get("id", "")),
        "title": doc.get("title", ""),
        "body": {"content": _render_content(model)},
        "documentStyle": {
            "background": {"color": {}},
            "defaultHeaderId": "",
            "defaultFooterId": "",
        },
        "suggestionsViewMode": "PREVIEW_WITHOUT_SUGGESTIONS",
    }
    inline_objects = doc.get("inlineObjects")
    if inline_objects != None and len(inline_objects) > 0:
        out["inlineObjects"] = inline_objects
    lists = doc.get("lists")
    if lists != None and len(lists) > 0:
        out["lists"] = lists
    return out
