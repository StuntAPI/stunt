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

# _require_bearer returns None if a Bearer token is present (authorized),
# or a 401 response if the header is missing. Microsoft Graph checks token
# PRESENCE (any non-empty Bearer token is accepted by this mock).
def _require_bearer(req):
    tok = _bearer(req)
    if tok == "":
        return respond(401, {
            "error": {
                "code": "InvalidAuthenticationToken",
                "message": "Access token is missing or invalid.",
            },
        })
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

# _odata_link builds an @odata.nextLink URL for OData pagination.
def _odata_link(base_url, skip):
    return base_url + "?$skip=" + str(skip)

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

# _apply_odata applies $select, $filter, $top, and $skip query parameters
# to a list of entities and returns an OData response envelope dict.
# base_url is used for the @odata.nextLink.
def _apply_odata(entities, query, base_url):
    # $filter
    filter_expr = query.get("$filter", "")
    entities = _filter_list(entities, filter_expr)

    # $select
    select_fields = _split_commas(query.get("$select", ""))

    # $skip
    skip = _to_int(query.get("$skip", "0"))

    # $top
    top = _to_int(query.get("$top", "0"))

    # Apply skip then top.
    if skip > 0 and skip < len(entities):
        entities = entities[skip:]
    elif skip >= len(entities):
        entities = []

    has_next = False
    if top > 0:
        if len(entities) > top:
            has_next = True
            entities = entities[:top]

    # Project selected fields.
    value = []
    for e in entities:
        value.append(_select_fields(e, select_fields))

    envelope = {
        "@odata.context": "https://graph.microsoft.com/v1.0/$metadata#collection",
        "value": value,
    }
    if has_next:
        envelope["@odata.nextLink"] = _odata_link(base_url, skip + top)
    return respond(200, envelope)

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
