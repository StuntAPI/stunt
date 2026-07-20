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

# Shopify IDs are large numeric integers. The collection store requires the
# "id" field to be a string, so we store as strings and convert back to int
# in the view functions for JSON responses.
_BASE_ID = 7000000000000

# _next_id returns a monotonically-increasing numeric ID (as a string for
# collection storage). Shopify IDs are large integers; we offset from a base.
def _next_id(kind):
    n = store_kv_incr("shopify", kind + "_seq")
    return str(_BASE_ID + n)

# _num_id converts a stored string id back to an int for JSON responses
# (Shopify returns numeric ids in REST/GraphQL responses).
def _num_id(s):
    return _to_int(s)

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
        "phone": "+10000000001",
        "created_at": _now(),
        "updated_at": _now(),
        "state": "enabled",
        "verified_email": True,
    })

    oc = store_collection("orders")
    oc.insert({
        "id": _next_id("orders"),
        "email": "buyer1@example.com",
        "financial_status": "paid",
        "fulfillment_status": None,
        "total_price": "89.99",
        "currency": "USD",
        "line_items": [
            {"id": _next_id("line_items"), "title": "Classic Leather Boots", "quantity": 1, "price": "89.99", "sku": "BOOTS-001"},
        ],
        "customer": {"id": _next_id("customers"), "email": "customer1@example.com"},
        "created_at": _now(),
        "updated_at": _now(),
        "order_number": 1001,
        "name": "#1001",
    })


def _make_product(pid, title, ptype, price, sku):
    return {
        "id": pid,
        "title": title,
        "product_type": ptype,
        "body_html": "<p>Synthetic product description.</p>",
        "vendor": "Stunt Store",
        "status": "active",
        "created_at": _now(),
        "updated_at": _now(),
        "variants": [
            {"id": pid, "product_id": pid, "title": "Default", "price": price, "sku": sku, "inventory_quantity": 100},
        ],
    }

# _now returns a synthetic ISO-8601 timestamp.
def _now():
    return "2024-06-15T10:30:00-04:00"

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
