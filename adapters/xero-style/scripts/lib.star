# Shared library for xero-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Xero auth: OAuth2 Bearer token + xero-tenant-id header on API calls.
# /connections only requires bearer; all /api.xro/* calls also need tenant.

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _tenant_id extracts the xero-tenant-id header (case-insensitive).
def _tenant_id(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    v = headers.get("xero-tenant-id")
    if v != None:
        return v
    target = "xero-tenant-id"
    for k in headers:
        if k.lower() == target:
            return headers[k]
    return ""

# _require_auth validates the Bearer token. Returns None if authorized,
# or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token == "":
        return _xero_err(401, "Unauthorized", "TokenExpired", "The access token has expired or is invalid")
    return None

# _require_tenant validates the xero-tenant-id header. Must be called after
# _require_auth. Returns None if present, or an error response if not.
def _require_tenant(req):
    tid = _tenant_id(req)
    if tid == "":
        return _xero_err(400, "BadRequest", "TenantRequired", "The xero-tenant-id header is required")
    return None

# _xero_err returns a Xero-style error response.
# Shape: { ErrorNumber, Type, Message }
def _xero_err(status, type_, error_number, message):
    return respond(status, {
        "ErrorNumber": error_number,
        "Type": type_,
        "Message": message,
    })

# _xero_err_elements returns a Xero-style validation error with Elements.
# Shape: { ErrorNumber, Type, Message, Elements: [...] }
def _xero_err_elements(status, type_, error_number, message, elements):
    return respond(status, {
        "ErrorNumber": error_number,
        "Type": type_,
        "Message": message,
        "Elements": elements,
    })

# _contact_id generates a Xero ContactID (GUID-style).
def _contact_id():
    n = store_kv_incr("xero", "contact_seq")
    return _guid(n)

# _invoice_id generates a Xero InvoiceID.
def _invoice_id():
    n = store_kv_incr("xero", "invoice_seq")
    return _guid(n + 1000)

# _payment_id generates a Xero PaymentID.
def _payment_id():
    n = store_kv_incr("xero", "payment_seq")
    return _guid(n + 2000)

# _guid generates a synthetic GUID-like string from a sequence number.
def _guid(n):
    hexchars = "0123456789abcdef"
    s = ""
    val = n
    if val == 0:
        s = "0"
    while val > 0:
        s = hexchars[val % 16] + s
        val = val // 16
    # Pad to 32 chars.
    while len(s) < 32:
        s = "0" + s
    # Format as 8-4-4-4-12.
    return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]

# _xero_id returns the Id field for the Xero envelope.
def _xero_id():
    n = store_kv_incr("xero", "envelope_seq")
    return _guid(n + 5000)

# _envelope returns Xero's { Id, Status, <Entities>: [...] } envelope.
# next_page is an optional Xero page-number cursor; when not None it is surfaced
# as a top-level "nextPage" field so callers can round-trip the next page.
def _envelope(entities_key, entities_list, next_page=None):
    body = {
        "Id": _xero_id(),
        "Status": "OK",
        entities_key: entities_list,
    }
    if next_page != None:
        body["nextPage"] = next_page
    return respond(200, body)

# _to_int parses a string/int into an int, returning 0 on any failure (mirrors
# the adapter's defensive parsing conventions).
def _to_int(v):
    if type(v) == type(0):
        return v
    if v == None:
        return 0
    s = str(v)
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _get_query reads a single query-param value from req, returning "" for a
# missing param or a None value (mirrors the adapter's existing conventions).
def _get_query(req, key):
    q = req.get("query")
    if q == None:
        return ""
    v = q.get(key, "")
    if v == None:
        return ""
    return v

# _list_page applies Xero-style pagination to a list of docs via the pure
# paginate() builtin. Xero paginates with a 1-based `page` query param together
# with `pageSize`. When `pageSize` is missing or <= 0 paging is DISABLED (the
# whole list is returned with a None next_page, preserving the prior
# unpaginated behavior). The returned next_page is the next 1-based page number
# (the value the client echoes back as the `page` query param), or None when no
# further pages remain. The builtin's opaque offset cursor is derived from the
# requested page number and never escapes the adapter.
def _list_page(req, docs):
    page_size = _to_int(_get_query(req, "pageSize"))
    if page_size <= 0:
        return docs, None
    page = _to_int(_get_query(req, "page"))
    if page <= 0:
        page = 1
    offset = (page - 1) * page_size
    page_docs, next_offset = paginate(docs, page_size, str(offset))
    next_page = None
    if next_offset != None:
        next_page = str(page + 1)
    return page_docs, next_page

# _ensure_accounts seeds default chart of accounts.
def _ensure_accounts():
    c = store_collection("accounts")
    docs = c.list()
    if len(docs) > 0:
        return
    defaults = [
        {"AccountID": _guid(101), "Code": "200", "Name": "Sales", "Type": "REVENUE", "Status": "ACTIVE", "Class": "REVENUE"},
        {"AccountID": _guid(102), "Code": "400", "Name": "Advertising", "Type": "EXPENSE", "Status": "ACTIVE", "Class": "EXPENSE"},
        {"AccountID": _guid(103), "Code": "090", "Name": "Bank Account", "Type": "BANK", "Status": "ACTIVE", "Class": "ASSET"},
    ]
    for a in defaults:
        c.insert(a)

# _contact_public returns the Xero-shaped contact object.
def _contact_public(doc):
    return {
        "ContactID": doc.get("ContactID", ""),
        "ContactStatus": doc.get("ContactStatus", "ACTIVE"),
        "Name": doc.get("Name", ""),
        "EmailAddress": doc.get("EmailAddress", ""),
        "IsSupplier": doc.get("IsSupplier", False),
        "IsCustomer": doc.get("IsCustomer", True),
    }

# _invoice_public returns the Xero-shaped invoice object.
def _invoice_public(doc):
    return {
        "InvoiceID": doc.get("InvoiceID", ""),
        "InvoiceNumber": doc.get("InvoiceNumber", ""),
        "Type": doc.get("Type", "ACCREC"),
        "Status": doc.get("Status", "DRAFT"),
        "Contact": doc.get("Contact", {}),
        "Date": doc.get("Date", "2024-06-15T00:00:00"),
        "DueDate": doc.get("DueDate", "2024-07-15T00:00:00"),
        "LineItems": doc.get("LineItems", []),
        "Total": doc.get("Total", "0.00"),
        "AmountDue": doc.get("AmountDue", "0.00"),
        "AmountPaid": doc.get("AmountPaid", "0.00"),
    }

# _payment_public returns the Xero-shaped payment object.
def _payment_public(doc):
    return {
        "PaymentID": doc.get("PaymentID", ""),
        "Invoice": doc.get("Invoice", {}),
        "Amount": doc.get("Amount", "0.00"),
        "Date": doc.get("Date", "2024-06-15T00:00:00"),
    }
