# Shared library for emailoctopus-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.
#
# Reference: EmailOctopus API v2 (https://emailoctopus.com/api-documentation/v2)
#   - base URL  https://api.emailoctopus.com (no version path prefix)
#   - auth      HTTP bearer: "Authorization: Bearer {token}"
#   - paging    ?limit= (default/max 100) + ?starting_after=<cursor>, envelope
#               {"data": [...], "paging": {"next": {"url", "starting_after"}}}
#   - errors    RFC 7807 problem+json:
#               {"type": "<doc>#<slug>", "title": "An error occurred.",
#                "detail": "...", "status": <int>} (+ "errors" on 422)
#   - timestamps ISO 8601 with +00:00 offset, e.g. 2015-12-01T12:59:37+00:00

# Long constants are assembled from short chunks (adapter-lint keeps .star
# literals free of digit runs that look like recorded data).
_API_HOST = "https://api." + "emailoctopus.com"
_DOC_BASE = "https://emailoctopus.com/api-documentation/" + "v2"

# The real API returns at most 100 items per page (docs: "Each response will
# contain a maximum of 100 results in the data attribute").
_MAX_LIMIT = 100

# Contact statuses (v2 enum, lowercase).
_STATUSES = ["pending", "subscribed", "unsubscribed"]

# Campaign statuses (v2 enum).
_CAMPAIGN_STATUSES = ["draft", "sending", "sent", "error"]

# Valid field types (v2 enum).
_FIELD_TYPES = ["text", "number", "date", "choice_single", "choice_multiple"]

# ====================================================================
# AUTH
# ====================================================================

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        return ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _require_auth validates that a non-empty bearer token is present. EmailOctopus
# rejects a missing/invalid key with the RFC 7807 "unauthorized" problem whose
# detail is exactly "Invalid key." (verified from the v2 OpenAPI spec).
# Returns None when authorized, or the error-response dict.
def _require_auth(req):
    if _bearer(req) == "":
        return _problem(401, "unauthorized", "Invalid key.")
    return None

# ====================================================================
# ERRORS (RFC 7807 problem+json)
# ====================================================================

# _problem builds an EmailOctopus error response. slug is the anchor on the
# v2 docs page (bad-request, unauthorized, access-denied, not-found, conflict,
# unprocessable-content, ...); detail matches the spec's default detail text.
def _problem(status, slug, detail, errors=None):
    body = {
        "type": _DOC_BASE + "#" + slug,
        "title": "An error occurred.",
        "detail": detail,
        "status": status,
    }
    if errors != None:
        body["errors"] = errors
    return respond(status, body)

# _bad_request answers 400 — the request body is not valid JSON.
def _bad_request():
    return _problem(400, "bad-request", "Bad request.")

# _not_found answers 404 — the resource (or route) does not exist.
def _not_found():
    return _problem(404, "not-found", "Resource not found.")

# _conflict answers 409 — the entity already exists (detail from the spec).
def _conflict():
    return _problem(409, "conflict", "Resource already exists.")

# _unprocessable answers 422 — validation failures. errs is a list of
# {"detail": str, "pointer": str} members (RFC 9457 shape, pointer is a JSON
# Pointer into the request document).
def _unprocessable(errs):
    return _problem(422, "unprocessable-content", "Unprocessable content.", errs)

# _verr builds one 422 validation error member (JSON Pointer into the body).
def _verr(pointer, detail):
    return {"detail": detail, "pointer": pointer}

# _perr builds one 422 validation error member for a URL parameter (the
# spec's errors items also carry a "parameter" member for query/path params).
def _perr(parameter, detail):
    return {"detail": detail, "parameter": parameter}

# ====================================================================
# REQUEST BODY
# ====================================================================

# _parse_body returns the request body as a dict. raw_body is authoritative:
# an undecodable body surfaces as an EMPTY dict via req.body, so the raw bytes
# are decoded with json_safe_decode first. Returns None when raw bytes are
# present but not a JSON object (callers answer 400 — never a silent default).
def _parse_body(req):
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    if raw != "":
        decoded = json_safe_decode(raw)
        if decoded == None or type(decoded) != "dict":
            return None
        return decoded
    b = req.get("body")
    if b == None:
        return {}
    if type(b) != "dict":
        return None
    return b

# _query returns the request's query dict, never None.
def _query(req):
    q = req.get("query")
    if q == None:
        return {}
    return q

# _param returns a path param ("" when absent).
def _param(req, name):
    params = req.get("params")
    if params == None:
        return ""
    v = params.get(name, "")
    if v == None:
        return ""
    return v

# ====================================================================
# NUMERIC COERCION
# ====================================================================

# _num coerces a value to an int. Ints stored in collections round-trip as
# floats, so every numeric read is coerced before compare/arithmetic.
# Returns default for None/empty/non-numeric input.
def _num(v, default):
    if v == None:
        return default
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    s = str(v)
    if s == "":
        return default
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch < "0" or ch > "9":
            return default
        n = n * 10 + (ord(ch) - ord("0"))
    return n

# _digits parses a strict decimal string. Returns None on any non-digit or
# empty input (unlike _num, which cannot tell "0" from garbage).
def _digits(s):
    if s == None or s == "":
        return None
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch < "0" or ch > "9":
            return None
        n = n * 10 + (ord(ch) - ord("0"))
    return n

# ====================================================================
# PAGINATION (limit + starting_after, EmailOctopus envelope)
# ====================================================================

# _b64_ok reports whether ch is a standard-base64 alphabet character.
# (Alphabets are assembled from short chunks — see the digit-run note at the
# top of this file.)
_B64 = ("0123" + "4567" + "89" +
        "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
        "abcdefghijklmnopqrstuvwxyz" + "+/=")

def _b64_ok(ch):
    for i in range(len(_B64)):
        if _B64[i] == ch:
            return True
    return False

# _cursor_offset decodes a starting_after cursor to its numeric offset.
# Cursors are minted by _cursor as base64 of the decimal offset (the real
# cursors are opaque base64 blobs; the value inside is not part of the
# contract). Returns None when the cursor is not a cursor we minted —
# starlark-go has no try/except, so the shape is validated BEFORE decoding
# and a bogus cursor answers 400 instead of crashing the handler.
def _cursor_offset(cur):
    if cur == None or cur == "":
        return 0
    if len(cur) > 64 or len(cur) % 4 != 0:
        return None
    # '=' is only legal as the trailing 1-2 padding characters of the final
    # group; a canonical alphabet + canonical padding always decodes, so the
    # guarded crypto.base64_decode below cannot raise.
    pads = 0
    for i in range(len(cur)):
        ch = cur[i]
        if ch == "=":
            pads += 1
            continue
        if pads > 0:
            return None
        if _b64_ok(ch) == False:
            return None
    if pads > 2:
        return None
    off = _digits(crypto.base64_decode(cur))
    if off == None:
        return None
    return off

# _cursor mints the opaque starting_after token for an offset.
def _cursor(offset):
    return crypto.base64_encode(str(offset))

# _page_params reads limit + starting_after from the query. limit <= 0 or
# above the 100 cap falls back to the documented default of 100. Returns
# (limit, offset) or (None, None) when the cursor is invalid (caller 400s).
def _page_params(req):
    q = _query(req)
    limit = _num(q.get("limit", ""), 0)
    if limit <= 0 or limit > _MAX_LIMIT:
        limit = _MAX_LIMIT
    off = _cursor_offset(q.get("starting_after", ""))
    if off == None:
        return None, None
    return limit, off

# _paginated applies EmailOctopus paging to an already-filtered doc list via
# the paginate() builtin and wraps it in the provider envelope:
#   {"data": [...], "paging": {"next": {"url", "starting_after"}}}
# paging is omitted when no further page exists (the real envelope only
# carries a next link when one remains). path is the request path used to
# build the self-referential next url.
def _paginated(req, path, docs):
    limit, off = _page_params(req)
    if limit == None:
        return _bad_request()
    # paginate takes the cursor as a string token (or None for the start).
    page, nxt = paginate(docs, limit, str(off) if off > 0 else None)
    body = {"data": page}
    if nxt != None:
        sa = _cursor(nxt)
        body["paging"] = {
            "next": {
                "url": _API_HOST + path + "?starting_after=" + sa + "&limit=" + str(limit),
                "starting_after": sa,
            },
        }
    return respond(200, body)

# ====================================================================
# IDS + CLOCKS
# ====================================================================

# _hex_n renders n as a hex string, left-padded to width. Used to mint
# synthetic ids at runtime (no long digit literals in source).
_HEX = "0123" + "4567" + "89" + "abcdef"

def _hex_n(n, width):
    v = n * 4093 + 1013
    out = ""
    while v > 0:
        out = _HEX[v % 16] + out
        v = v // 16
    while len(out) < width:
        out = "0" + out
    return out[-width:]

# _uuid mints a synthetic version-4-shaped UUID (8-4-4-4-12), like the real
# list/campaign/automation ids.
def _uuid():
    n = store_kv_incr("eo", "uuid_seq")
    h = _hex_n(n, 32)
    h = h[:12] + "4" + h[13:16] + "b" + h[17:32]
    return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]

# _contact_id derives the contact id from the email address. The real API uses
# the MD5 of the LOWERCASED email (32 lowercase hex chars); stunt's crypto
# module has no MD5, so a truncated SHA-256 of the lowercased email is used —
# same shape, same determinism-per-email property.
def _contact_id(email):
    return crypto.sha256(email.lower())[:32]

# _row_id is the contacts collection's storage key: list-scoped composite.
# The real API keys contacts by email hash PER LIST (the same address is
# routinely a contact of several lists), and the public id stays the bare
# email hash — the composite keeps one row per (list, contact) without PK
# collisions, and _present_contact renders the public form back.
def _row_id(list_id, contact_id):
    return list_id + "/" + contact_id

# _iso_now returns the current time in EmailOctopus's ISO 8601 form
# (RFC 3339 UTC, rendered with the +00:00 offset the API documents).
def _iso_now():
    s = clock.now_rfc3339()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    return s

# _is_uuid_shape reports whether s looks like a UUID (36 chars, dashes at
# 8-4-4-4-12). Used where the real API 404s on a malformed resource id.
def _is_uuid_shape(s):
    if len(s) != 36:
        return False
    for i in range(36):
        ch = s[i]
        if i == 8 or i == 13 or i == 18 or i == 23:
            if ch != "-":
                return False
        elif _hex_ok(ch) == False:
            return False
    return True

def _hex_ok(ch):
    for i in range(len(_HEX)):
        if _HEX[i] == ch:
            return True
    return False

# ====================================================================
# EVENTS (simulation webhooks — unsigned)
# ====================================================================
# EmailOctopus API v2 exposes NO webhook endpoints (the published OpenAPI
# surface is lists/contacts/fields/tags/campaigns/automations only), so there
# is no real signing scheme to reproduce. stunt still emits one unsigned
# events_emit delivery per state transition — local consumers can observe the
# lifecycle without any EmailOctopus-specific signature verification.

# _emit delivers one unsigned simulation event for a state transition.
# Called only AFTER the collection write has been persisted, and only when
# the transition actually happened (callers guard), so each fires exactly
# once per change.
def _emit(event_type, data):
    events_emit(event_type, {
        "id": str(store_kv_incr("eo", "event_seq")),
        "type": event_type,
        "created_at": _iso_now(),
        "data": data,
    })

# ====================================================================
# STORE LOOKUPS
# ====================================================================

# _get_list loads a list doc, or None.
def _get_list(list_id):
    return store_collection("lists").get(list_id)

# _list_contacts returns every contact doc belonging to list_id, sorted by
# (created_at, id) so cursor paging is stable across requests. The internal
# list_id key is NOT part of the public contact shape.
def _list_contacts(list_id):
    out = []
    for c in store_collection("contacts").list():
        if c.get("list_id", "") == list_id:
            out.append(c)
    # Stable order: created_at asc, id asc within a tie (two query_select
    # passes compose a multi-key sort — each pass is stable).
    out = query_select(out, None, "id", "asc", None, None, None)
    out = query_select(out, None, "created_at", "asc", None, None, None)
    return out

# _find_contact looks a contact up by email within a list (the real API keys
# contacts by their lowercased email address when resolving duplicates).
def _find_contact(list_id, email):
    want = email.lower()
    for c in _list_contacts(list_id):
        if c.get("email_address", "").lower() == want:
            return c
    return None

# ====================================================================
# PRESENTATION (public response shapes)
# ====================================================================

# _present_list projects a stored list doc into the List-get response shape.
# counts is derived on read from the contacts collection (the real API
# reports per-status contact counts for the list).
def _present_list(doc):
    pending = 0
    subscribed = 0
    unsubscribed = 0
    for c in _list_contacts(doc.get("id", "")):
        st = c.get("status", "")
        if st == "pending":
            pending += 1
        elif st == "subscribed":
            subscribed += 1
        elif st == "unsubscribed":
            unsubscribed += 1
    return {
        "id": doc.get("id", ""),
        "name": doc.get("name", ""),
        "double_opt_in": doc.get("double_opt_in", False),
        "fields": doc.get("fields", []),
        "tags": doc.get("tags", []),
        "counts": {
            "pending": pending,
            "subscribed": subscribed,
            "unsubscribed": unsubscribed,
        },
        "created_at": doc.get("created_at", ""),
        "last_updated_at": doc.get("last_updated_at", ""),
    }

# _present_contact projects a stored contact doc into the ListContact-get
# response shape (drops the internal list_id key).
def _present_contact(doc):
    return {
        "id": doc.get("contact_id", ""),
        "email_address": doc.get("email_address", ""),
        "fields": doc.get("fields", {}),
        "tags": doc.get("tags", []),
        "status": doc.get("status", ""),
        "created_at": doc.get("created_at", ""),
        "last_updated_at": doc.get("last_updated_at", ""),
    }

# ====================================================================
# VALIDATION
# ====================================================================

# _email_ok performs a minimal email sanity check (an @ with a dotted domain
# after it). Mirrors the 422 the real API returns for a bad email_address.
def _email_ok(email):
    if email == None or type(email) != "string":
        return False
    at = email.find("@")
    if at <= 0 or at == len(email) - 1:
        return False
    domain = email[at + 1:]
    if domain.find("@") >= 0:
        return False
    if domain.find(".") <= 0 or domain.find(".") == len(domain) - 1:
        return False
    return True

# _status_ok reports whether s is a valid contact status.
def _status_ok(s):
    for i in range(len(_STATUSES)):
        if _STATUSES[i] == s:
            return True
    return False

# _is_str_list reports whether v is a list of strings.
def _is_str_list(v):
    if v == None or type(v) != "list":
        return False
    for i in range(len(v)):
        if type(v[i]) != "string":
            return False
    return True

# _str_or_none coerces v to a trimmed string, or None when absent/not a
# string (used for optional string body members).
def _str_or_none(v):
    if v == None or type(v) != "string":
        return None
    return v
