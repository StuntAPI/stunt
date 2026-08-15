# Files handlers — Starlark stateful logic backed by store_blob (content)
# and store_collection (metadata).
#
# Each handler receives `req` with keys: method, path, headers, body, params, query.
# Returns respond(status, body, headers).

# _next_id returns a monotonically-increasing provider-style ID using the
# KV store as a sequence counter. Produces ids like "file_1", "file_2", ...
def _next_id(prefix):
    # Atomic increment via store_kv_incr (race-free under concurrent requests).
    return prefix + "_" + str(store_kv_incr("drive", prefix + "_seq"))

# _now returns a synthetic ISO-8601 timestamp. The value is fixed for
# determinism in local testing.
def _now():
    return "2024-01-15T12:00:00Z"

# POST /upload/drive/v3/files — create a file or folder.
#
# Accepts BOTH request shapes:
#  - JSON body  {"name","content","mimeType","parents"}       (convenience form)
#  - a real simple media upload: raw bytes as the request body with
#    ?uploadType=media&name=<filename> (Content-Type application/octet-stream).
#    The raw bytes arrive in req["raw_body"]; the name comes from the query.
#
# For a folder: set body.mimeType to "application/vnd.google-apps.folder".
# Folders have no blob content — only metadata.
def on_upload(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    body = req["body"]
    if body == None:
        body = {}
    query = req.get("query")
    if query == None:
        query = {}
    raw = req.get("raw_body")
    if raw == None:
        raw = ""

    mime_type = body.get("mimeType", "application/octet-stream")
    # Name precedence: JSON body, then ?name= query, then a default.
    name = body.get("name", None)
    if name == None:
        name = query.get("name", "untitled")
    file_id = _next_id("file")

    is_folder = mime_type == "application/vnd.google-apps.folder"

    if is_folder:
        size = 0
    else:
        # Content precedence: JSON body.content, else the raw request body
        # (a real octet-stream media upload).
        content = body.get("content", None)
        if content == None:
            content = raw
        b = store_blob("drive")
        b.put(file_id, content)
        size = len(content)

    doc = {
        "id": file_id,
        "name": name,
        "mimeType": mime_type,
        "size": size,
        "createdTime": _now(),
        "modifiedTime": _now(),
        "trashed": False,
    }
    if "parents" in body:
        doc["parents"] = body["parents"]

    c = store_collection("files")
    c.insert(doc)
    _record_change(file_id, doc, False)
    return respond(201, doc)

# POST /drive/v3/files — create file/folder METADATA (no content upload).
# Used for folder creation during parent resolution. JSON body
# {"name","mimeType","parents"} -> 200 with the created resource (incl id).
def on_create_metadata(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    body = req["body"]
    if body == None:
        body = {}
    name = body.get("name", "untitled")
    mime_type = body.get("mimeType", "application/vnd.google-apps.folder")
    file_id = _next_id("file")
    doc = {
        "id": file_id,
        "name": name,
        "mimeType": mime_type,
        "size": 0,
        "createdTime": _now(),
        "modifiedTime": _now(),
        "trashed": False,
    }
    if "parents" in body:
        doc["parents"] = body["parents"]
    store_collection("files").insert(doc)
    _record_change(file_id, doc, False)
    return respond(200, doc)

# GET /drive/v3/files/{id} — retrieve file metadata, or download content
# if ?alt=media is present.
def on_get(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    id = req["params"]["id"]
    c = store_collection("files")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "File not found: " + id, "code": 404}})

    # Check for alt=media query param to download content.
    query = req.get("query", None)
    if query != None and query.get("alt", None) == "media":
        if doc.get("mimeType", None) == "application/vnd.google-apps.folder":
            return respond(400, {"error": {"message": "Cannot download folder: " + id, "code": 400}})
        b = store_blob("drive")
        content = b.get(id)
        if content == None:
            return respond(404, {"error": {"message": "Content not found for file: " + id, "code": 404}})
        return respond(200, content, {"Content-Type": doc.get("mimeType", "application/octet-stream")})

    return respond(200, doc)

# GET /drive/v3/files — list files (metadata only).
# Honors the real Drive files.list params the mock can back: q, orderBy and
# fields (see _apply_file_filters), plus pageSize/pageToken paging.
def on_list(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    c = store_collection("files")
    docs = c.list()
    visible, qerr = _apply_file_filters(req, docs)
    if qerr != "":
        return _drive_err(400, "Invalid query filter: " + qerr, "INVALID_ARGUMENT")
    # Apply Drive-style paging (pageSize / pageToken) after filtering.
    page, next_token = _list_page(req, visible)
    result = {"files": page}
    if next_token != None:
        result["nextPageToken"] = next_token
    return respond(200, result)

# --- list helpers ---

# _apply_file_filters maps the real Drive files.list params the mock stores
# can honor onto query_select, applied before paging like the real API:
#   q       -> the Drive q grammar subset (see _q_parse): clauses on name,
#              mimeType, trashed, parents and modifiedTime/createdTime
#              joined with "and"/"or". Unparseable q is a 400 (returned as
#              the second result).
#   orderBy -> comma-separated keys; the first recognized one wins
#              (createdTime, modifiedTime, name, quotaBytesUsed -> size),
#              each with an optional "desc"/"asc" suffix.
#   fields  -> a "files(k1,k2,...)" selection projects each file object.
# Trashed files are excluded by default (like real Drive); an explicit
# trashed clause in q overrides that.
# Returns (selected docs, "" ) or (None, error message).
def _apply_file_filters(req, docs):
    q = _get_query(req).get("q", "")
    if q == None:
        q = ""

    if q.strip() == "":
        base = []
        for d in docs:
            if not d.get("trashed", False):
                base.append(d)
    else:
        clauses, ops, qerr = _q_parse(q)
        if qerr != "":
            return None, qerr
        explicit_trashed = False
        for cl in clauses:
            if cl["field"] == "trashed":
                explicit_trashed = True
        base = []
        for d in docs:
            if explicit_trashed or not d.get("trashed", False):
                base.append(d)
        base = _q_select(base, clauses, ops)

    order_by, order_dir = _order_by(req)
    return query_select(base, None, order_by, order_dir, None, None, _fields_for(req)), ""

# --- Drive q grammar (subset) ---
#
# Grammar (case-sensitive keywords, like real Drive):
#
#   query  := clause ( ("and" | "or") clause )*
#   clause := field op value
#   field  := name | mimeType | trashed | parents | modifiedTime |
#             createdTime | <dotted.property.path>
#   op     := = | != | < | <= | > | >= | contains | in
#   value  := 'single-quoted literal (\' and \\ escapes)' |
#             true | false        (trashed only)
#
# "and"/"or" groups are evaluated with or-over-and precedence: clauses in
# an and-run are AND'ed (via query_select filter triples); or-runs union.
# The "parents in 'id'" clause checks membership in the file's parents
# list (evaluated directly; query_select's "in" tests the reverse
# direction). Unparseable or out-of-subset queries return an error string
# which the handler maps to a 400.

# _q_word_char reports whether ch can appear in a bare word (field names,
# keywords, true/false).
def _q_word_char(ch):
    return (ch >= "a" and ch <= "z") or (ch >= "A" and ch <= "Z") or (ch >= "0" and ch <= "9") or ch == "." or ch == "_"

# _q_tokenize lexes a q string into tokens: {"k":"lit","v":s} for quoted
# literals, {"k":"word","v":w} for bare words, {"k":"op","v":o} for the
# symbolic operators. Returns (tokens, "") or (None, error).
def _q_tokenize(q):
    toks = []
    i = 0
    n = len(q)
    while i < n:
        ch = q[i]
        if ch == " " or ch == "\t":
            i = i + 1
        elif ch == "'" or ch == "\"":
            quote = ch
            i = i + 1
            buf = ""
            closed = False
            while i < n:
                c = q[i]
                if c == "\\" and i + 1 < n and (q[i + 1] == "\\" or q[i + 1] == "'"):
                    buf = buf + q[i + 1]
                    i = i + 2
                elif c == quote:
                    closed = True
                    i = i + 1
                    break
                else:
                    buf = buf + c
                    i = i + 1
            if not closed:
                return None, "unterminated string literal"
            toks.append({"k": "lit", "v": buf})
        elif ch == "=":
            toks.append({"k": "op", "v": "="})
            i = i + 1
        elif ch == "!":
            if i + 1 < n and q[i + 1] == "=":
                toks.append({"k": "op", "v": "!="})
                i = i + 2
            else:
                return None, "unexpected '!'"
        elif ch == "<" or ch == ">":
            if i + 1 < n and q[i + 1] == "=":
                toks.append({"k": "op", "v": ch + "="})
                i = i + 2
            else:
                toks.append({"k": "op", "v": ch})
                i = i + 1
        elif _q_word_char(ch):
            buf = ""
            while i < n and _q_word_char(q[i]):
                buf = buf + q[i]
                i = i + 1
            toks.append({"k": "word", "v": buf})
        else:
            return None, "unexpected character '" + ch + "'"
    return toks, ""

# _q_field_ok reports whether field is one of the supported clause fields
# (or a dotted custom-property path, resolved by query_select).
def _q_field_ok(field):
    if field == "name" or field == "mimeType" or field == "trashed" or field == "parents" or field == "modifiedTime" or field == "createdTime":
        return True
    return field.find(".") > 0

# _q_op_ok validates the (field, op) combination against the grammar
# subset. Returns "" or an error message.
def _q_op_ok(field, op):
    if field == "name" or field == "mimeType":
        if op == "=" or op == "!=" or op == "contains":
            return ""
        return "operator " + op + " not supported for " + field
    if field == "trashed":
        if op == "=":
            return ""
        return "operator " + op + " not supported for trashed"
    if field == "parents":
        if op == "in":
            return ""
        return "operator " + op + " not supported for parents"
    # modifiedTime / createdTime / dotted property paths: full comparisons.
    if op == "=" or op == "!=" or op == "<" or op == "<=" or op == ">" or op == ">=":
        return ""
    return "operator " + op + " not supported for " + field

# _q_parse parses a full q string into (clauses, ops, "") where clauses is
# the ordered list of {"field","op","value"} dicts and ops the "and"/"or"
# joiners between them (len(ops) == len(clauses)-1). Any deviation from
# the grammar subset returns (None, None, error).
def _q_parse(q):
    toks, err = _q_tokenize(q)
    if err != "":
        return None, None, err
    clauses = []
    ops = []
    i = 0
    n = len(toks)
    expect_clause = True
    while i < n:
        t = toks[i]
        if expect_clause:
            if t["k"] != "word":
                return None, None, "expected a field name"
            field = t["v"]
            if not _q_field_ok(field):
                return None, None, "unsupported field '" + field + "'"
            i = i + 1
            if i >= n:
                return None, None, "expected an operator after '" + field + "'"
            t2 = toks[i]
            if t2["k"] == "op":
                op = t2["v"]
            elif t2["k"] == "word" and (t2["v"] == "contains" or t2["v"] == "in"):
                op = t2["v"]
            else:
                return None, None, "expected an operator after '" + field + "'"
            operr = _q_op_ok(field, op)
            if operr != "":
                return None, None, operr
            i = i + 1
            if i >= n:
                return None, None, "expected a value after '" + field + " " + op + "'"
            t3 = toks[i]
            if field == "trashed":
                if t3["k"] != "word" or not (t3["v"] == "true" or t3["v"] == "false"):
                    return None, None, "trashed requires true or false"
                val = t3["v"] == "true"
            elif op == "in":
                if t3["k"] != "lit":
                    return None, None, "'in' requires a quoted literal value"
                val = t3["v"]
            else:
                if t3["k"] != "lit":
                    return None, None, "expected a quoted literal value"
                val = t3["v"]
            clauses.append({"field": field, "op": op, "value": val})
            expect_clause = False
            i = i + 1
        else:
            if t["k"] == "word" and (t["v"] == "and" or t["v"] == "or"):
                ops.append(t["v"])
                expect_clause = True
                i = i + 1
            else:
                return None, None, "expected 'and' or 'or'"
    if len(clauses) == 0:
        return None, None, "empty query"
    if expect_clause:
        return None, None, "dangling operator"
    return clauses, ops, ""

# _q_group_matches evaluates one and-run of clauses over docs. Fields that
# map onto query_select triples are AND'ed through query_select's filter;
# "parents in 'id'" clauses are applied directly on top (list membership).
def _q_group_matches(docs, group):
    triples = []
    for cl in group:
        if cl["field"] != "parents":
            triples.append([cl["field"], cl["op"], cl["value"]])
    cur = docs
    if len(triples) > 0:
        cur = query_select(cur, triples, "", "", None, None, None)
    for cl in group:
        if cl["field"] == "parents":
            want = cl["value"]
            nxt = []
            for d in cur:
                ps = d.get("parents", None)
                if ps != None and want in ps:
                    nxt.append(d)
            cur = nxt
    return cur

# _q_select evaluates the parsed (clauses, ops) query over docs with
# or-over-and precedence: and-runs filter (query_select), or unions,
# preserving first-seen order and de-duplicating ids.
def _q_select(docs, clauses, ops):
    groups = []
    cur = [clauses[0]]
    for i in range(len(ops)):
        if ops[i] == "and":
            cur.append(clauses[i + 1])
        else:
            groups.append(cur)
            cur = [clauses[i + 1]]
    groups.append(cur)
    seen = {}
    out = []
    for g in groups:
        for d in _q_group_matches(docs, g):
            if d["id"] not in seen:
                seen[d["id"]] = True
                out.append(d)
    return out

# _order_by parses the Drive orderBy param and returns (field, dir) for the
# first recognized key, or ("", "") when none apply.
def _order_by(req):
    ob = _get_query(req).get("orderBy", "")
    if ob == None or ob == "":
        return "", ""
    for seg in ob.split(","):
        seg = seg.strip()
        dir = "asc"
        if seg.endswith(" desc"):
            dir = "desc"
            seg = seg[:len(seg) - len(" desc")].strip()
        elif seg.endswith(" asc"):
            seg = seg[:len(seg) - len(" asc")].strip()
        field = ""
        if seg == "createdTime" or seg == "modifiedTime" or seg == "name":
            field = seg
        elif seg == "quotaBytesUsed":
            field = "size"
        if field != "":
            return field, dir
    return "", ""

# _fields_for parses a Drive fields param like "files(id,name)" (optionally
# with sibling selections such as ",nextPageToken") into the list of keys
# each file object is projected onto, or None when no projection applies.
def _fields_for(req):
    fields = _get_query(req).get("fields", "")
    if fields == None or fields == "":
        return None
    idx = fields.find("files(")
    if idx < 0:
        return None
    rest = fields[idx + len("files("):]
    end = rest.find(")")
    if end < 0:
        return None
    names = []
    for part in rest[:end].split(","):
        part = part.strip()
        if part != "" and part != "*":
            names.append(part)
    if len(names) == 0:
        return None
    return names

# PATCH /drive/v3/files/{id} — update file metadata (e.g., name, trashed).
def on_patch(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    id = req["params"]["id"]
    c = store_collection("files")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "File not found: " + id, "code": 404}})

    body = req["body"]
    if body != None:
        for k in body:
            # mimeType changes are allowed (metadata update).
            # content changes via PATCH are not part of this MVP.
            doc[k] = body[k]
    doc["modifiedTime"] = _now()
    c.update(id, doc)
    _record_change(id, doc, False)
    return respond(200, doc)

# DELETE /drive/v3/files/{id} — permanently delete a file (content + metadata).
def on_delete(req):
    auth = _require_auth(req)
    if auth != None:
        return auth
    id = req["params"]["id"]
    c = store_collection("files")
    doc = c.get(id)
    if doc == None:
        return respond(404, {"error": {"message": "File not found: " + id, "code": 404}})

    # Delete blob content if it exists (idempotent for folders with no content).
    if doc.get("mimeType", None) != "application/vnd.google-apps.folder":
        b = store_blob("drive")
        b.delete(id)

    c.delete(id)
    _record_change(id, None, True)
    return respond(204, None)
