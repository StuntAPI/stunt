# Shared library for microsoft-graph-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _auth_fault returns the Graph 401 error envelope for a missing, unknown,
# or expired Bearer token.
def _auth_fault():
    return respond(401, {
        "error": {
            "code": "InvalidAuthenticationToken",
            "message": "Access token is missing or invalid.",
        },
    })

# _seed_known_tokens inserts (once, guarded by a KV flag) the static mock
# tokens that engine tests present, into the "tokens" collection. This
# adapter models only the Graph data plane (the token is minted by the
# identity platform, i.e. entra-id-style), so the known-token bootstrap is
# the stand-in for "a token the tenant previously issued". Far-future
# expiry so static-token tests keep passing; unknown tokens still 401.
def _seed_known_tokens():
    if store_kv_get("graph", "tokens_seeded") == "yes":
        return
    store_kv_set("graph", "tokens_seeded", "yes")
    tc = store_collection("tokens")
    # get-then-insert: concurrent cold-start requests must not collide on the PK
    if tc.get("mock-bearer-token") == None:
        tc.insert({
            "id": "mock-bearer-token",
            "expires_at": clock.now_unix() + 3600*24*365,
        })

# _require_bearer returns None if the request carries a Bearer token that is
# known to the store and not expired, or a 401 response otherwise (missing
# header, unknown token, or expired token) with the Graph error envelope.
def _require_bearer(req):
    tok = _bearer(req)
    if tok == "":
        return _auth_fault()
    _seed_known_tokens()
    tc = store_collection("tokens")
    doc = tc.get(tok)
    if doc == None:
        return _auth_fault()
    exp = doc.get("expires_at", 0)
    if exp != None and exp != 0 and clock.now_unix() > exp:
        return _auth_fault()
    return None

# _err returns a Microsoft Graph error envelope.
def _err(code, status, message):
    return respond(status, {
        "error": {
            "code": code,
            "message": message,
        },
    })

# _ok returns a 200 response with an @odata.context envelope.
def _ok(context, value):
    return respond(200, {
        "@odata.context": context,
        "value": value,
    })

# _odata_link builds an @odata.nextLink URL for OData pagination. It carries
# both $top (so the next page keeps the same page size) and $skip (the plain
# numeric offset of the next page — Graph's documented OData semantics).
# extra, when non-empty, is appended as additional query parameters (e.g.
# calendarView's startDateTime/endDateTime window).
def _odata_link(base_url, top, skip_token, extra = ""):
    link = base_url + "?$top=" + str(top) + "&$skip=" + skip_token
    if extra != "":
        link = link + "&" + extra
    return link

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
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

# _pad6 zero-pads a number to 6 digits.
def _pad6(n):
    s = str(n)
    while len(s) < 6:
        s = "0" + s
    return s

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _split_commas splits a comma-separated string into a list (no spaces).
def _split_commas(s):
    if s == None or s == "":
        return []
    parts = []
    current = ""
    for i in range(len(s)):
        ch = s[i]
        if ch == ",":
            current = _strip(current)
            if current != "":
                parts.append(current)
            current = ""
        else:
            current = current + ch
    current = _strip(current)
    if current != "":
        parts.append(current)
    return parts

# _strip removes leading and trailing whitespace.
def _strip(s):
    start = 0
    end = len(s)
    while start < end:
        ch = s[start]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            start = start + 1
        else:
            break
    while end > start:
        ch = s[end - 1]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            end = end - 1
        else:
            break
    return s[start:end]

# _select_fields projects only the requested fields from an entity dict.
# select_fields is a list of field names. Returns a new dict containing
# only those keys that exist in the entity.
def _select_fields(entity, select_fields):
    if len(select_fields) == 0:
        return entity
    out = {}
    for f in select_fields:
        v = entity.get(f)
        if v != None:
            out[f] = v
    return out

# _filter_list applies a simple OData $filter pattern-match to a list of
# entities. Supports the pattern: field eq 'value' or field eq 'value'.
# Returns the filtered list.
def _filter_list(entities, filter_expr):
    if filter_expr == None or filter_expr == "":
        return entities
    # Parse "field eq 'value'" — split on " eq "
    eq_idx = _find_substr(filter_expr, " eq ")
    if eq_idx < 0:
        return entities
    field = _strip(filter_expr[:eq_idx])
    rest = _strip(filter_expr[eq_idx + 4:])
    # Extract the value between single quotes.
    val = rest
    if len(rest) >= 2 and rest[0] == "'":
        end_q = _find_substr(rest[1:], "'")
        if end_q >= 0:
            val = rest[1:1 + end_q]
    result = []
    for e in entities:
        ev = e.get(field, "")
        if str(ev) == val:
            result.append(e)
    return result

# _find_substr returns the index of the first occurrence of needle in s,
# or -1 if not found.
def _find_substr(s, needle):
    if len(needle) == 0:
        return 0
    for i in range(len(s) - len(needle) + 1):
        match = True
        for j in range(len(needle)):
            if s[i + j] != needle[j]:
                match = False
                break
        if match:
            return i
    return -1

# _page_params parses the OData $top/$skip query values. In Graph both are
# plain non-negative decimal integers ($skip is a 0-based offset, NOT an
# opaque cursor); anything else is a 400 back to the client. Returns
# (top, skip, ok); an absent parameter defaults to 0.
def _page_params(query):
    top_s = query.get("$top", "")
    skip_s = query.get("$skip", "")
    if top_s != None and top_s != "" and not _is_digits(top_s):
        return 0, 0, False
    if skip_s != None and skip_s != "" and not _is_digits(skip_s):
        return 0, 0, False
    return _to_int(top_s), _to_int(skip_s), True

# _list_page applies OData numeric paging to a (pre-filtered) list of
# entities: $skip is a plain 0-based offset and $top the page size, so
# ?$skip=10 returns the tail from offset 10 even without $top. Returns
# (page, next_skip, ok): next_skip is the numeric offset of the next page
# ("" when none remains) and ok is False when $top/$skip are malformed.
def _list_page(entities, query):
    top, skip, ok = _page_params(query)
    if not ok:
        return [], "", False
    total = len(entities)
    if skip > total:
        skip = total
    if top > 0:
        page, next_cursor = paginate(entities, top, str(skip))
        if next_cursor == None:
            next_cursor = ""
        return page, next_cursor, True
    return entities[skip:], "", True

# _apply_odata applies $select, $filter, $top, and $skip query parameters
# to a list of entities and returns an OData response envelope dict.
# base_url is used for the @odata.nextLink; extra carries any additional
# query parameters the nextLink must preserve (e.g. a calendarView window).
# $filter runs BEFORE paging; paging is numeric (see _list_page).
def _apply_odata(entities, query, base_url, extra = ""):
    # $filter (apply before paging).
    filter_expr = query.get("$filter", "")
    entities = _filter_list(entities, filter_expr)

    # $select projection fields.
    select_fields = _split_commas(query.get("$select", ""))

    # Pagination: $top = page size, $skip = plain numeric offset.
    top = _to_int(query.get("$top", ""))
    page, next_cursor, ok = _list_page(entities, query)
    if not ok:
        return _err("invalidRequest", 400, "$top and $skip must be non-negative integers.")

    # Project selected fields.
    value = []
    for e in page:
        value.append(_select_fields(e, select_fields))

    envelope = {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#collection",
        "value": value,
    }
    if next_cursor != None and next_cursor != "":
        envelope["@odata.nextLink"] = _odata_link(base_url, top, next_cursor, extra)
    return respond(200, envelope)

# --- Change notifications (webhook subscriptions) -------------------------
#
# Microsoft Graph has NO webhook signature: notification payloads are not
# HMAC-signed. The documented verification mechanism is the OPTIONAL
# `clientState` value supplied at subscription creation and echoed verbatim
# in every notification — receivers must compare it against their expected
# secret and drop mismatches. stunt reproduces this model (unsigned-by-design)
# and additionally simulates the subscription validation handshake (see
# scripts/subscriptions.star).
#
# Notification envelope (Graph shape):
#   {"value": [{
#      "subscriptionId": "...",
#      "changeType": "created" | "updated" | "deleted",
#      "clientState": "<echoed from subscription>",
#      "subscriptionExpirationDateTime": "...",
#      "resource": "me/events",
#      "resourceData": {"@odata.type": "#Microsoft.Graph.Event",
#                       "@odata.id": "me/events/evt-000001",
#                       "id": "evt-000001"},
#      "tenantId": "..."
#   }]}

# _normalize_resource strips a leading "/" so "me/events" and "/me/events"
# match the same subscription.
def _normalize_resource(r):
    if r == None:
        return ""
    if len(r) > 0 and r[0] == "/":
        return r[1:]
    return r

# _notify_subscriptions delivers a change notification for (resource,
# change_type) to the first subscription created for that exact resource whose
# changeType list includes the change. Graph does not deliver to
# non-matching subscriptions. odata_type is the "#Microsoft.Graph.X" type and
# resource_id the changed entity's id.
def _notify_subscriptions(change_type, resource, odata_type, resource_id):
    sc = store_collection("subscriptions")
    normalized = _normalize_resource(resource)
    for s in sc.list():
        if _normalize_resource(s.get("resource", "")) != normalized:
            continue
        types = _split_commas(s.get("changeType", ""))
        if change_type not in types:
            continue
        notification = {
            "subscriptionId": s.get("id", ""),
            "changeType": change_type,
            "clientState": s.get("clientState", ""),
            "subscriptionExpirationDateTime": s.get("expirationDateTime", ""),
            "resource": normalized,
            "resourceData": {
                "@odata.type": odata_type,
                "@odata.id": normalized + "/" + resource_id,
                "id": resource_id,
            },
            "tenantId": s.get("tenantId", "mock-tenant"),
        }
        events_emit("changeNotification", {"value": [notification]})
        return

# --- OneDrive driveItem helpers (shared by drive.star and drive_upload.star) ---

_DRIVE_ID = "b!mock-drive-id-0001"

# _seed_files inserts the default OneDrive root children once (three items:
# a Word doc, an Excel workbook, and a Reports folder). Excel workbook
# handlers call it too, so a workbook table can be resolved against a real
# drive item.
def _seed_files():
    fc = store_collection("files")
    docs = fc.list()
    if len(docs) > 0:
        return
    seed_files = [
        {
            "id": "file-000001-doc",
            "name": "Project Plan.docx",
            "file": {"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
            "folder": None,
            "size": 24576,
            "parentId": "root",
            "createdDateTime": "2024-03-01T10:00:00Z",
            "lastModifiedDateTime": "2024-06-10T15:30:00Z",
        },
        {
            "id": "file-000002-xls",
            "name": "Budget.xlsx",
            "file": {"mimeType": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
            "folder": None,
            "size": 53248,
            "parentId": "root",
            "createdDateTime": "2024-02-15T09:00:00Z",
            "lastModifiedDateTime": "2024-06-12T11:00:00Z",
        },
        {
            "id": "folder-000001-reports",
            "name": "Reports",
            "file": None,
            "folder": {"childCount": 5},
            "size": 0,
            "parentId": "root",
            "createdDateTime": "2024-01-20T08:00:00Z",
            "lastModifiedDateTime": "2024-06-14T16:00:00Z",
        },
    ]
    for f in seed_files:
        fc.insert(f)

# _is_digits reports whether s is a non-empty run of ASCII digits.
def _is_digits(s):
    if s == None or len(s) == 0:
        return False
    for i in range(len(s)):
        ch = s[i]
        if ch < "0" or ch > "9":
            return False
    return True

# _next_item_id mints a monotonic driveItem id.
def _next_item_id():
    return "item-" + _pad6(store_kv_incr("drive", "item_seq"))

# _strip_colon strips the trailing ':' of a colon-addressed path segment
# ("photo.jpg:" → "photo.jpg"). Returns None if the segment does not end
# with ':' — the caller should reject the request as malformed addressing.
def _strip_colon(seg):
    if seg == None or len(seg) < 2 or seg[-1:] != ":":
        return None
    return seg[:-1]

# _find_child_by_name returns the doc under parent_id with the given name,
# or None.
def _find_child_by_name(fc, parent_id, name):
    for d in fc.list():
        if d.get("parentId", "root") == parent_id and d.get("name", "") == name:
            return d
    return None

# _conflict_rename returns the first free " (n)"-suffixed variant of name
# under parent_id ("photo.jpg" → "photo (1).jpg").
def _conflict_rename(fc, parent_id, name):
    dot = name.rfind(".")
    stem = name
    ext = ""
    if dot > 0:
        stem = name[:dot]
        ext = name[dot:]
    n = 1
    for _ in range(1000):
        candidate = stem + " (" + str(n) + ")" + ext
        if _find_child_by_name(fc, parent_id, candidate) == None:
            return candidate
        n = n + 1
    return stem + " (" + str(n) + ")" + ext

# _parent_ref builds the parentReference facet for a parentId.
def _parent_ref(parent_id):
    path = "/drive/root:"
    if parent_id != "root":
        fc = store_collection("files")
        parent = fc.get(parent_id)
        if parent != None:
            path = "/drive/root:/" + parent.get("name", "")
    return {
        "driveId": _DRIVE_ID,
        "driveType": "business",
        "id": parent_id,
        "path": path,
    }

# _drive_item builds the public driveItem JSON from a stored files doc.
# Absent facets (file/folder) are omitted, matching real Graph responses.
def _drive_item(doc):
    item = {
        "id": doc["id"],
        "name": doc["name"],
        "size": doc.get("size", 0),
        "parentReference": _parent_ref(doc.get("parentId", "root")),
        "createdDateTime": doc.get("createdDateTime", "2024-01-01T00:00:00Z"),
        "lastModifiedDateTime": doc.get("lastModifiedDateTime", "2024-01-01T00:00:00Z"),
    }
    if doc.get("file") != None:
        item["file"] = doc["file"]
    if doc.get("folder") != None:
        item["folder"] = doc["folder"]
    return item

# _me returns the constant mock "me" profile used by /me and as the sender
# for mail/calendar. This mock uses a fixed identity so tests can assert
# stable fields.
def _me():
    return {
        "id": "a1b2c3d4-0001-0001-0001-000000000001",
        "displayName": "Alex Mockerman",
        "givenName": "Alex",
        "surname": "Mockerman",
        "mail": "alex@mock-tenant.onmicrosoft.com",
        "userPrincipalName": "alex@mock-tenant.onmicrosoft.com",
        "jobTitle": "Software Engineer",
        "mobilePhone": "+1 555-0100",
        "businessPhones": ["+1 555-0101"],
        "officeLocation": "Building A/1",
        "preferredLanguage": "en-US",
        "accountEnabled": True,
    }
