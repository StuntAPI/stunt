# Shared library for shopify-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# ============================================================================
# SHOPIFY WEBHOOK SIGNATURE SCHEME (DOCUMENTATION)
# ============================================================================
# Shopify signs every webhook delivery with an HMAC-SHA256 of the raw request
# body using the shop's API secret key. The signature is sent base64-encoded
# in the header:
#
#   X-Shopify-Hmac-SHA256: <base64(HMAC-SHA256(api_secret_key, raw_body))>
#
# To verify on the receiving end (your webhook handler):
#
#   1. Read the RAW request body (bytes, before any JSON parsing).
#   2. Compute HMAC-SHA256 with your Shopify API secret key as the key.
#   3. Base64-encode the digest.
#   4. Compare against the X-Shopify-Hmac-SHA256 header (constant-time).
#
# In Go:
#   mac := hmac.New(sha256.New, []byte(apiSecretKey))
#   mac.Write(rawBody)
#   expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
#   if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Shopify-Hmac-SHA256"))) {
#       return 401 // invalid signature
#   }
#
# IMPORTANT: Webhooks MUST be acknowledged with a 200 OK and an EMPTY body.
# Shopify retries deliveries that don't get a 200 within ~5 seconds, and
# after enough failures will disable the webhook subscription.
#
# OAuth install callback validation: during the OAuth flow, Shopify redirects
# to your callback URL with query params including `hmac`, `shop`, `code`,
# `timestamp`, and others. The `hmac` param = hex(HMAC-SHA256(api_secret_key,
# querystring_with_hmac_removed_and_params_sorted))). Verify it before
# exchanging the code for an access token.
# ============================================================================

# Mock webhook signing secret. Configure your Shopify webhook receiver with
# this exact string to verify stunt's deliveries. Public + low-entropy: local
# stunt only — never reuse outside the simulator.
_WEBHOOK_SECRET = "shpss_stunt_mock_api_client_secret"

# _signed_emit MACs the exact on-wire body and delivers with
# X-Shopify-Hmac-SHA256. The same (topic, payload) feeds events_body (signing
# input) and events_emit (delivery), so the signature verifies against the bytes
# the sink receives. Shopify uses base64 (NOT hex).
def _signed_emit(topic, payload):
    body = events_body(topic, payload)
    sig = crypto.hmac_sha256(_WEBHOOK_SECRET, body, encoding="base64")
    events_emit(topic, payload, {"X-Shopify-Hmac-SHA256": sig})

# _require_token validates the X-Shopify-Access-Token header. Returns None
# if authorized, or a 401 error-response dict to return from the handler if
# the token is missing or empty.
def _require_token(req):
    headers = req.get("headers")
    if headers == None:
        return _unauthorized()
    token = headers.get("X-Shopify-Access-Token", "")
    if token == None or token == "":
        return _unauthorized()
    return None

# _unauthorized returns a Shopify-style 401 error response.
def _unauthorized():
    return respond(401, {"errors": "[API] Invalid API key or access token (unrecognized login or wrong account or password)"})

# _shopify_err returns a Shopify-style error envelope: {"errors": "..."}.
def _shopify_err(status_code, message):
    return respond(status_code, {"errors": message})

# _not_found returns a Shopify-style 404 for a missing resource.
def _not_found(resource, id):
    return respond(404, {"errors": resource + " not found: " + str(id)})

# _emit_if_subscribed emits a signed webhook event (X-Shopify-Hmac-SHA256) if
# any registered subscription matches the topic. At most one delivery per
# call, matching Shopify's one-subscription-per-topic model.
def _emit_if_subscribed(topic, payload):
    wc = store_collection("webhooks")
    hooks = wc.list()
    for h in hooks:
        if h.get("topic", "") == topic:
            _signed_emit(topic, payload)
            return

# _cents parses a money value into integer cents. Accepts decimal strings
# ("89.99" — the common wire form), ints, and floats (JSON numbers), so
# arithmetic on amounts never goes through binary floats for string inputs.
def _cents(v):
    if v == None:
        return 0
    t = type(v)
    if t == "int":
        return v * 100
    if t == "float":
        return int(v * 100 + 0.5)
    s = str(v).strip()
    if s == "":
        return 0
    parts = s.split(".")
    whole = _to_int(parts[0])
    frac = 0
    if len(parts) > 1:
        fs = parts[1]
        if len(fs) > 2:
            fs = fs[:2]
        elif len(fs) == 1:
            fs = fs + "0"
        frac = _to_int(fs)
    return whole * 100 + frac

# _fmt_money formats integer cents back into a Shopify-style decimal string
# ("89.99"). Inverse of _cents.
def _fmt_money(cents):
    neg = False
    if cents < 0:
        neg = True
        cents = -cents
    whole = cents // 100
    frac = cents % 100
    fs = str(frac)
    if frac < 10:
        fs = "0" + fs
    out = str(whole) + "." + fs
    if neg:
        return "-" + out
    return out

# Shopify IDs are large numeric integers. The collection store requires the
# "id" field to be a string, so we store as strings and convert back to int
# in the view functions for JSON responses.
# Assembled at runtime (no long digit literals): 7 followed by 12 zeros.
_BASE_ID = 7 * 1000 * 1000 * 1000 * 1000

# _next_id returns a monotonically-increasing numeric ID (as a string for
# collection storage). Shopify IDs are large integers; we offset from a base.
def _next_id(kind):
    n = store_kv_incr("shopify", kind + "_seq")
    return str(_BASE_ID + n)

# _num_id converts a stored or inbound id to an int for JSON responses
# (Shopify returns numeric ids in REST/GraphQL responses). TOTAL over the
# shapes an id can take: stored string, JSON int, JSON float (the engine
# decodes numbers to float when a client sends an id as a number) — a
# plain string parser raised on the latter two and 500'd the response.
def _num_id(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(str(v))

# _customer_id_numeric coerces an embedded customer object's id (typed SDKs
# unmarshal order.customer.id as int64).
def _customer_id_numeric(c):
    if c == None:
        return None
    if c.get("id", None) == None:
        return c
    out = dict(c)
    out["id"] = _num_id(c["id"])
    return out

# _seed populates default products, orders, and customers on first access so
# that list endpoints return realistic data without prior setup.
def _seed():
    if store_kv_get("shopify", "seeded") == "yes":
        return
    store_kv_set("shopify", "seeded", "yes")

    pc = store_collection("products")
    pc.insert(_make_product(_next_id("products"), "Classic Leather Boots", "footwear", "89.99", "BOOTS-001"))
    pc.insert(_make_product(_next_id("products"), "Organic Cotton Hoodie", "apparel", "54.00", "HOOD-002"))

    cc = store_collection("customers")
    cc.insert({
        "id": _next_id("customers"),
        "email": "customer1@example.com",
        "first_name": "Jane",
        "last_name": "Doe",
        "orders_count": 3,
        "total_spent": "234.50",
        "phone": "+" + str(10 * 1000 * 1000 * 1000 + 1),
        "created_at": _now(),
        "updated_at": _now(),
        "state": "enabled",
        "verified_email": True,
    })

    oc = store_collection("orders")
    seeded_oid = _next_id("orders")
    oc.insert({
        "id": seeded_oid,
        "email": "buyer1@example.com",
        "financial_status": "paid",
        "fulfillment_status": None,
        "total_price": "89.99",
        "currency": "USD",
        "line_items": [
            {"id": _next_id("line_items"), "title": "Classic Leather Boots", "quantity": 1, "price": "89.99", "sku": "BOOTS-001", "variant_id": None, "_fulfilled": 0},
        ],
        "customer": {"id": _next_id("customers"), "email": "customer1@example.com"},
        "created_at": _now(),
        "updated_at": _now(),
        "order_number": 1001,
        "name": "#1001",
        "closed_at": None,
        "cancelled_at": None,
        "cancel_reason": None,
    })
    # Consume order number 1001 from the sequence so created orders continue
    # from #1002.
    store_kv_incr("shopify", "order_numbers")

    # The seeded order's "paid" status is derived from a real sale
    # transaction, so financial_status arithmetic is uniform everywhere.
    tc = store_collection("transactions")
    tc.insert({
        "id": _next_id("transactions"),
        "order_id": seeded_oid,
        "kind": "sale",
        "amount": "89.99",
        "status": "success",
        "currency": "USD",
        "created_at": _now(),
    })


def _make_product(pid, title, ptype, price, sku):
    return {
        "id": pid,
        "title": title,
        "product_type": ptype,
        "body_html": "<p>Synthetic product description.</p>",
        "vendor": "Stunt Store",
        "status": "active",
        "tags": "",
        "created_at": _now(),
        "updated_at": _now(),
        "variants": [
            {"id": pid, "product_id": pid, "title": "Default", "price": price, "sku": sku, "inventory_quantity": 100},
        ],
    }

# _now returns the current time as an ISO-8601 timestamp (live clock —
# created_at/updated_at reflect request time; seeded docs are stamped once
# at seed time and stay stable).
def _now():
    return clock.now_rfc3339()

# _parse_gql_stub inspects a GraphQL query string and returns a tuple
# (kind, arg_str) where kind is the top-level operation keyword (e.g.
# "products", "orders", "customer") and arg_str is unused. It uses simple
# substring matching — no full GraphQL parser.
def _parse_gql_stub(query):
    if query == None:
        return "", ""
    q = query.lower()
    if "products" in q:
        return "products", ""
    if "orders" in q:
        return "orders", ""
    if "customer" in q:
        return "customers", ""
    if "shop" in q:
        return "shop", ""
    return "", ""

# _strip_json removes a trailing ".json" suffix from a path param value.
# Shopify routes use .json suffixes, but the route matcher captures the
# entire segment including the suffix.
def _strip_json(s):
    if s == None:
        return ""
    if s.endswith(".json"):
        return s[:-5]
    return s

# _to_int parses a decimal string to int. Returns 0 for None or empty.
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

# _list_page applies Shopify-style cursor pagination to a list of docs via the
# pure paginate() builtin. The `limit` query param sets the page size; when it
# is missing or <= 0 paging is DISABLED (the whole list is returned with a None
# next_cursor, preserving the prior unpaginated behavior). The `page_info`
# query param is the opaque cursor token returned by a prior call (None/"" for
# the first page). Returns (page, next_cursor) where next_cursor is the opaque
# token for the next page, or None when done. Shopify surfaces continuation via
# a 'Link: <url>; rel="next"' header built by _next_link.
def _list_page(req, docs):
    limit = _to_int(_get_query(req, "limit"))
    cursor = _get_query(req, "page_info")
    return paginate(docs, limit, cursor)

# _next_link builds a Shopify-style 'Link: <url>; rel="next"' header value
# carrying the next-page cursor (and the same limit), or None when there is no
# further page. The client round-trips the page_info token (plus limit) as
# query params on the returned URL.
def _next_link(req, next_cursor, limit):
    if next_cursor == None:
        return None
    # The proxy sets X-Forwarded-Proto; port mode is plain http on
    # loopback. Clients that FOLLOW the page_info URL (not just parse the
    # token) need the real scheme.
    scheme = req.get("headers", {}).get("x-forwarded-proto", "")
    if scheme == None or scheme == "":
        host0 = req.get("host", "") or ""
        scheme = "http" if host0.startswith("127.0.0.1") or host0.startswith("localhost") else "https"
    host = req.get("host", "")
    if host == None:
        host = ""
    path = req.get("path", "")
    if path == None:
        path = ""
    url = scheme + "://" + host + path + "?page_info=" + next_cursor
    if limit > 0:
        url = url + "&limit=" + str(limit)
    return "<" + url + '>; rel="next"'

# ── REST payload views (shared: handler scripts AND the GraphQL resolvers
# emit these for webhooks — receivers get ONE schema per topic, regardless
# of which surface triggered the change) ─────────────────────────────────────


def _line_item_view(li):
    qty = li.get("quantity", 0)
    ful = li.get("_fulfilled", 0)
    if ful > qty:
        ful = qty
    remaining = qty - ful
    line_status = None
    if ful > 0:
        if remaining == 0:
            line_status = "fulfilled"
        else:
            line_status = "partial"
    vid = li.get("variant_id", None)
    if vid != None:
        vid = _num_id(str(vid))
    return {
        "id": _num_id(li["id"]),
        "title": li.get("title", ""),
        "quantity": qty,
        "price": li.get("price", "0.00"),
        "sku": li.get("sku", ""),
        "variant_id": vid,
        "fulfillable_quantity": remaining,
        "fulfillment_status": line_status,
    }


def _order_view(o):
    lines = o.get("line_items", [])
    line_views = []
    for li in lines:
        line_views.append(_line_item_view(li))
    return {
        "id": _num_id(o["id"]),
        "email": o.get("email", ""),
        "financial_status": o.get("financial_status", "pending"),
        "fulfillment_status": o.get("fulfillment_status", None),
        "total_price": o.get("total_price", "0.00"),
        "currency": o.get("currency", "USD"),
        "line_items": line_views,
        "customer": _customer_id_numeric(o.get("customer", {})),
        "order_number": o.get("order_number", 0),
        "name": o.get("name", ""),
        "closed_at": o.get("closed_at", None),
        "cancelled_at": o.get("cancelled_at", None),
        "cancel_reason": o.get("cancel_reason", None),
        "created_at": o.get("created_at", _now()),
        "updated_at": o.get("updated_at", _now()),
    }


def _product_view(p):
    return {
        "id": _num_id(p["id"]),
        "title": p.get("title", ""),
        "product_type": p.get("product_type", ""),
        "body_html": p.get("body_html", ""),
        "vendor": p.get("vendor", ""),
        "status": p.get("status", "active"),
        "tags": p.get("tags", ""),
        "created_at": p.get("created_at", _now()),
        "updated_at": p.get("updated_at", _now()),
        "variants": _variants_numeric(p.get("variants", [])),
    }

# _variants_numeric coerces variant id/product_id to numeric at render —
# seeded variants store string ids and typed SDKs unmarshal them as ints.
def _variants_numeric(vs):
    out = []
    for v in vs:
        w = dict(v)
        if w.get("id", None) != None:
            w["id"] = _num_id(w["id"])
        if w.get("product_id", None) != None:
            w["product_id"] = _num_id(w["product_id"])
        out.append(w)
    return out


def _customer_view(c):
    return {
        "id": _num_id(c["id"]),
        "email": c.get("email", ""),
        "first_name": c.get("first_name", ""),
        "last_name": c.get("last_name", ""),
        "orders_count": c.get("orders_count", 0),
        "total_spent": c.get("total_spent", "0.00"),
        "phone": c.get("phone", ""),
        "note": c.get("note", ""),
        "tags": c.get("tags", ""),
        "state": c.get("state", "enabled"),
        "verified_email": c.get("verified_email", True),
        "created_at": c.get("created_at", _now()),
        "updated_at": c.get("updated_at", _now()),
    }
