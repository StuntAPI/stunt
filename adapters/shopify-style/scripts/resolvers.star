# Shopify Admin GraphQL resolvers — served by the engine's real GraphQL
# executor at POST /admin/api/2024-10/graphql.json (see adapter.yaml).
#
# Root fields use on_<field>(callArg); object fields use
# resolve_<Type>_<field>(callArg). Scalar fields fall back to the default
# resolver (parent[fieldName]). Objects map onto the same collections as the
# REST surface (products/orders/customers/...), with gid:// global IDs on the
# GraphQL side and snake_case REST fields translated per type. Connections
# (edges/nodes/pageInfo) paginate via query_select with offset cursors.
#
# All data is synthetic.

# ---------------------------------------------------------------------------
# gid helpers
# ---------------------------------------------------------------------------

# _gid renders a Shopify global ID.
def _gid(kind, num):
    return "gid://shopify/" + kind + "/" + str(num)

# _gid_num extracts the numeric tail of a gid (or a bare id, which passes
# through unchanged so plain REST ids keep working).
def _gid_num(gid):
    s = str(gid)
    idx = -1
    for i in range(len(s)):
        if s[i] == "/":
            idx = i
    if idx >= 0:
        return s[idx + 1:]
    return s

# ---------------------------------------------------------------------------
# Shared small helpers
# ---------------------------------------------------------------------------

# _int_arg coerces an Int argument to int: inline literals arrive as ints
# but JSON variables arrive as floats (json.Unmarshal decodes numbers into
# float64). lib.star's _to_int only parses strings.
def _int_arg(v):
    if v == None:
        return None
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(str(v))

# _money renders a Shopify Money object from a decimal-string amount.
def _money(amount, currency):
    return {"amount": amount, "currencyCode": currency}

# _upper uppercases an ASCII snake/lowercase stored value for enums
# (ProductStatus/CustomerState serialize uppercase in GraphQL).
def _upper(s):
    if s == None:
        return s
    return str(s).upper()

# _strip_html removes <...> tags from an HTML string (description vs
# descriptionHtml).
def _strip_html(s):
    if s == None:
        return None
    out = ""
    inside = False
    for i in range(len(s)):
        ch = s[i]
        if ch == "<":
            inside = True
        elif ch == ">":
            inside = False
        elif not inside:
            out = out + ch
    return out

# _slug derives a product handle from its title.
def _slug(title):
    if title == None:
        return ""
    s = ""
    for i in range(len(title)):
        ch = title[i]
        if ch == " ":
            s = s + "-"
        elif (ch >= "a" and ch <= "z") or (ch >= "0" and ch <= "9") or ch == "-":
            s = s + ch
        elif ch >= "A" and ch <= "Z":
            s = s + chr(ord(ch) + 32)
    return s

# _connection builds a Shopify connection object from an ordered item list
# with offset cursors (1-based end offsets, matching `after` semantics).
def _connection(items, first, after):
    total = len(items)
    offset = 0
    if after != None and after != "":
        offset = _to_int(after)
    # Int variables arrive as JSON floats — coerce before query_select.
    if first != None:
        first = _int_arg(first)
    page = query_select(items, None, None, "", first, offset, None)
    end = offset + len(page)

    edges = []
    for it in page:
        edges.append({"node": it, "cursor": str(offset + len(edges) + 1)})

    return {
        "edges": edges,
        "nodes": page,
        "pageInfo": {
            "hasNextPage": end < total,
            "hasPreviousPage": offset > 0,
            "startCursor": str(offset + 1) if len(page) > 0 else None,
            "endCursor": str(end) if len(page) > 0 else None,
        },
    }

# _lower lowercases an ASCII string (query matching is case-insensitive,
# like Shopify's search).
def _lower(s):
    if s == None:
        return ""
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if ch >= "A" and ch <= "Z":
            out = out + chr(ord(ch) + 32)
        else:
            out = out + ch
    return out

# _query_matches implements the subset of Shopify's `query` search syntax the
# adapter models: a bare term is a case-insensitive substring match against
# the given fields; "field:value" tokens require equality on that field.
def _query_matches(doc, q, fields, field_map):
    if q == None or q == "":
        return True
    for part in q.split(" "):
        part = part.strip()
        if part == "":
            continue
        if part.find(":") >= 0:
            kv = part.split(":", 1)
            key = field_map.get(_lower(kv[0]), None)
            if key == None:
                continue
            if _lower(str(doc.get(key, ""))) != _lower(kv[1]):
                return False
        else:
            needle = _lower(part)
            hit = False
            for f in fields:
                if needle in _lower(str(doc.get(f, ""))):
                    hit = True
                    break
            if not hit:
                return False
    return True

# ---------------------------------------------------------------------------
# Query root resolvers
# ---------------------------------------------------------------------------

# shop → Shop
def on_shop(args):
    _seed()
    return respond(200, {
        "id": _gid("Shop", 1),
        "name": "Stunt Dev Store",
        "myshopifyDomain": "stunt-dev.myshopify.com",
        "description": "Synthetic Shopify-style store for local testing",
    })

# product(id: gid) → Product | None
def on_product(args):
    _seed()
    pid = _gid_num(args["args"]["id"])
    doc = store_collection("products").get(pid)
    return respond(200, doc)

# products(first, after, query, sortKey, reverse) → ProductConnection
def on_products(args):
    _seed()
    a = args["args"]
    docs = store_collection("products").list()

    kept = []
    for p in docs:
        if _query_matches(p, a.get("query"), ["title", "vendor"], {"title": "title", "vendor": "vendor", "status": "status"}):
            kept.append(p)

    order_by = "id"
    if a.get("sortKey") == "TITLE":
        order_by = "title"
    elif a.get("sortKey") == "CREATED_AT":
        order_by = "created_at"
    elif a.get("sortKey") == "UPDATED_AT":
        order_by = "updated_at"
    direction = "desc" if a.get("reverse") == True else "asc"

    kept = query_select(kept, None, order_by, direction, None, None, None)
    return respond(200, _connection(kept, a["first"], a.get("after")))

# order(id: gid) → Order | None
def on_order(args):
    _seed()
    oid = _gid_num(args["args"]["id"])
    doc = store_collection("orders").get(oid)
    return respond(200, doc)

# orders(first, after, query) → OrderConnection
def on_orders(args):
    _seed()
    a = args["args"]
    kept = []
    for o in store_collection("orders").list():
        if _query_matches(o, a.get("query"), ["name", "email"], {"name": "name", "email": "email", "financial_status": "financial_status"}):
            kept.append(o)
    kept = query_select(kept, None, "id", "desc", None, None, None)
    return respond(200, _connection(kept, a["first"], a.get("after")))

# customer(id: gid) → Customer | None
def on_customer(args):
    _seed()
    cid = _gid_num(args["args"]["id"])
    doc = store_collection("customers").get(cid)
    if doc != None and doc.get("_archived", False):
        return respond(200, None)
    return respond(200, doc)

# customers(first, after, query, sortKey, reverse) → CustomerConnection
def on_customers(args):
    _seed()
    a = args["args"]
    kept = []
    for c in store_collection("customers").list():
        if c.get("_archived", False):
            continue
        if _query_matches(c, a.get("query"), ["email", "first_name", "last_name"], {"email": "email"}):
            kept.append(c)

    order_by = "id"
    if a.get("sortKey") == "CREATED_AT":
        order_by = "created_at"
    direction = "desc" if a.get("reverse") == True else "asc"

    kept = query_select(kept, None, order_by, direction, None, None, None)
    return respond(200, _connection(kept, a["first"], a.get("after")))

# ---------------------------------------------------------------------------
# Mutation root resolvers
# ---------------------------------------------------------------------------

# productCreate(input) → ProductCreatePayload. Mirrors the REST create
# semantics (default variant, default status/vendor) with Shopify's
# userErrors convention.
def on_productCreate(args):
    _seed()
    input = args["args"].get("input")
    if input == None:
        input = {}

    title = input.get("title", None)
    if title == None or str(title).strip() == "":
        return respond(200, {"product": None, "userErrors": [
            {"field": ["title"], "message": "Title can't be blank"},
        ]})

    pc = store_collection("products")
    pid = _next_id("products")

    variants = input.get("variants", None)
    built = []
    if variants != None and len(variants) > 0:
        for i in range(len(variants)):
            v = variants[i]
            if v == None:
                v = {}
            built.append({
                "id": str(_num_id(pid) + i + 1),
                "product_id": pid,
                "title": v.get("title", "Default Title"),
                "price": _fmt_money(_cents(v.get("price", "0.00"))),
                "sku": v.get("sku", "") or "",
                "inventory_quantity": v.get("inventoryQuantity", 0) or 0,
            })
    else:
        built.append({
            "id": str(_num_id(pid) + 1),
            "product_id": pid,
            "title": "Default Title",
            "price": "0.00",
            "sku": "",
            "inventory_quantity": 0,
        })

    tags = input.get("tags", None)
    tag_str = ""
    if tags != None:
        tag_str = ",".join(tags)

    prod = {
        "id": pid,
        "title": str(title),
        "product_type": input.get("productType", "") or "",
        "body_html": input.get("descriptionHtml", "") or "",
        "vendor": input.get("vendor", "Stunt Store") or "Stunt Store",
        "status": _lower_status(input.get("status", None), "active"),
        "tags": tag_str,
        "created_at": _now(),
        "updated_at": _now(),
        "variants": built,
    }
    pc.insert(prod)
    _emit_if_subscribed("products/create", _gql_product_view(prod))
    return respond(200, {"product": prod, "userErrors": []})

# productUpdate(input {id, ...}) → ProductUpdatePayload
def on_productUpdate(args):
    _seed()
    input = args["args"].get("input")
    if input == None:
        input = {}

    pc = store_collection("products")
    pid = _gid_num(input.get("id", ""))
    prod = pc.get(pid)
    if prod == None:
        return respond(200, {"product": None, "userErrors": [
            {"field": ["id"], "message": "Product not found"},
        ]})

    if input.get("title", None) != None:
        prod["title"] = input["title"]
    if input.get("descriptionHtml", None) != None:
        prod["body_html"] = input["descriptionHtml"]
    if input.get("vendor", None) != None:
        prod["vendor"] = input["vendor"]
    if input.get("productType", None) != None:
        prod["product_type"] = input["productType"]
    if input.get("tags", None) != None:
        prod["tags"] = ",".join(input["tags"])
    status = input.get("status", None)
    if status != None:
        lowered = _lower_status(status, None)
        if lowered == None:
            return respond(200, {"product": prod, "userErrors": [
                {"field": ["status"], "message": "Invalid status specified. Status must be active, archived, or draft"},
            ]})
        prod["status"] = lowered

    variants = input.get("variants", None)
    if variants != None:
        for v in variants:
            if v == None:
                continue
            vid = v.get("id", None)
            if vid == None:
                continue
            target = None
            for ev in prod.get("variants", []):
                if str(ev.get("id", "")) == _gid_num(vid):
                    target = ev
                    break
            if target == None:
                continue
            if v.get("title", None) != None:
                target["title"] = v["title"]
            if v.get("price", None) != None:
                target["price"] = _fmt_money(_cents(v["price"]))
            if v.get("sku", None) != None:
                target["sku"] = v["sku"]
            if v.get("inventoryQuantity", None) != None:
                target["inventory_quantity"] = v["inventoryQuantity"]

    prod["updated_at"] = _now()
    pc.update(pid, prod)
    _emit_if_subscribed("products/update", _gql_product_view(prod))
    return respond(200, {"product": prod, "userErrors": []})

# productDelete(id: gid) → ProductDeletePayload
def on_productDelete(args):
    _seed()
    pc = store_collection("products")
    pid = _gid_num(args["args"]["id"])
    prod = pc.get(pid)
    if prod == None:
        return respond(200, {"deletedProductId": None, "userErrors": [
            {"field": ["id"], "message": "Product not found"},
        ]})
    view = _gql_product_view(prod)
    pc.delete(pid)
    _emit_if_subscribed("products/delete", view)
    return respond(200, {"deletedProductId": _gid("Product", pid), "userErrors": []})

# customerCreate(input) → CustomerCreatePayload (REST parity: duplicate email
# is a userError).
def on_customerCreate(args):
    _seed()
    input = args["args"].get("input")
    if input == None:
        input = {}

    cc = store_collection("customers")
    email = input.get("email", "") or ""
    if email != "":
        for c in cc.list():
            if c.get("_archived", False):
                continue
            if c.get("email", "") == email:
                return respond(200, {"customer": None, "userErrors": [
                    {"field": ["email"], "message": "has already been taken"},
                ]})

    doc = {
        "id": _next_id("customers"),
        "email": email,
        "first_name": input.get("firstName", "") or "",
        "last_name": input.get("lastName", "") or "",
        "phone": input.get("phone", "") or "",
        "note": input.get("note", "") or "",
        "tags": "",
        "orders_count": 0,
        "total_spent": "0.00",
        "state": "enabled",
        "verified_email": True,
        "created_at": _now(),
        "updated_at": _now(),
    }
    cc.insert(doc)
    _emit_if_subscribed("customers/create", _gql_customer_view(doc))
    return respond(200, {"customer": doc, "userErrors": []})

# customerUpdate(input {id, ...}) → CustomerUpdatePayload
def on_customerUpdate(args):
    _seed()
    input = args["args"].get("input")
    if input == None:
        input = {}

    cc = store_collection("customers")
    cid = _gid_num(input.get("id", ""))
    cust = cc.get(cid)
    if cust == None or cust.get("_archived", False):
        return respond(200, {"customer": None, "userErrors": [
            {"field": ["id"], "message": "Customer not found"},
        ]})

    new_email = input.get("email", None)
    if new_email != None and new_email != "":
        for c in cc.list():
            if str(c["id"]) == str(cid) or c.get("_archived", False):
                continue
            if c.get("email", "") == new_email:
                return respond(200, {"customer": cust, "userErrors": [
                    {"field": ["email"], "message": "has already been taken"},
                ]})

    if new_email != None:
        cust["email"] = new_email
    if input.get("firstName", None) != None:
        cust["first_name"] = input["firstName"]
    if input.get("lastName", None) != None:
        cust["last_name"] = input["lastName"]
    if input.get("phone", None) != None:
        cust["phone"] = input["phone"]
    if input.get("note", None) != None:
        cust["note"] = input["note"]

    cust["updated_at"] = _now()
    cc.update(cid, cust)
    _emit_if_subscribed("customers/update", _gql_customer_view(cust))
    return respond(200, {"customer": cust, "userErrors": []})

# customerDelete(id: gid) → CustomerDeletePayload (REST parity: archives
# rather than hard-deleting).
def on_customerDelete(args):
    _seed()
    cc = store_collection("customers")
    cid = _gid_num(args["args"]["id"])
    cust = cc.get(cid)
    if cust == None or cust.get("_archived", False):
        return respond(200, {"deletedCustomerId": None, "userErrors": [
            {"field": ["id"], "message": "Customer not found"},
        ]})
    view = _gql_customer_view(cust)
    cust["_archived"] = True
    cust["updated_at"] = _now()
    cc.update(cid, cust)
    _emit_if_subscribed("customers/delete", view)
    return respond(200, {"deletedCustomerId": _gid("Customer", cid), "userErrors": []})

# orderCancel(orderId, reason, restock) → OrderCancelPayload. Mirrors the
# REST cancel: sets cancelled_at (+ reason), optionally restocks, and a
# second cancel is a userError.
def on_orderCancel(args):
    _seed()
    a = args["args"]
    oc = store_collection("orders")
    oid = _gid_num(a["orderId"])
    order = oc.get(oid)
    if order == None:
        return respond(200, {"order": None, "orderCancelUserErrors": [
            {"field": ["orderId"], "message": "Order not found"},
        ]})
    if order.get("cancelled_at", None) != None:
        return respond(200, {"order": order, "orderCancelUserErrors": [
            {"field": ["order"], "message": "Order has already been cancelled"},
        ]})

    reason = a.get("reason", None)
    reason_str = "other"
    if reason != None:
        reason_str = str(reason).lower()

    order["cancelled_at"] = _now()
    order["cancel_reason"] = reason_str
    order["updated_at"] = _now()
    if a.get("restock") == True:
        for li in order.get("line_items", []):
            remaining = li.get("quantity", 0) - li.get("_fulfilled", 0)
            if remaining > 0:
                _gql_restock_variant(li, remaining)
    oc.update(oid, order)
    _emit_if_subscribed("orders/cancelled", _gql_order_view(order))
    return respond(200, {"order": order, "orderCancelUserErrors": []})

# orderClose(orderId) → OrderClosePayload. A cancelled order cannot be closed.
def on_orderClose(args):
    _seed()
    oc = store_collection("orders")
    oid = _gid_num(args["args"]["orderId"])
    order = oc.get(oid)
    if order == None:
        return respond(200, {"order": None, "orderCloseUserErrors": [
            {"field": ["orderId"], "message": "Order not found"},
        ]})
    if order.get("cancelled_at", None) != None:
        return respond(200, {"order": order, "orderCloseUserErrors": [
            {"field": ["order"], "message": "Cannot close a cancelled order"},
        ]})
    if order.get("closed_at", None) == None:
        order["closed_at"] = _now()
        order["updated_at"] = _now()
        oc.update(oid, order)
        _emit_if_subscribed("orders/updated", _gql_order_view(order))
    return respond(200, {"order": order, "orderCloseUserErrors": []})

# ---------------------------------------------------------------------------
# Object resolvers — Product
# ---------------------------------------------------------------------------

def resolve_Product_id(args):
    return respond(200, _gid("Product", args["parent"]["id"]))

def resolve_Product_handle(args):
    return respond(200, _slug(args["parent"].get("title", "")))

def resolve_Product_descriptionHtml(args):
    return respond(200, args["parent"].get("body_html", ""))

def resolve_Product_description(args):
    return respond(200, _strip_html(args["parent"].get("body_html", "")))

def resolve_Product_status(args):
    return respond(200, _upper(args["parent"].get("status", "ACTIVE")))

def resolve_Product_productType(args):
    return respond(200, args["parent"].get("product_type", ""))

def resolve_Product_tags(args):
    tags = args["parent"].get("tags", "")
    out = []
    if tags != None and tags != "":
        for part in str(tags).split(","):
            part = part.strip()
            if part != "":
                out.append(part)
    return respond(200, out)

def resolve_Product_createdAt(args):
    return respond(200, args["parent"].get("created_at", ""))

def resolve_Product_updatedAt(args):
    return respond(200, args["parent"].get("updated_at", ""))

def resolve_Product_variants(args):
    first = args["args"].get("first")
    if first == None:
        first = 10
    return respond(200, _connection(args["parent"].get("variants", []), _int_arg(first), None))

# ---------------------------------------------------------------------------
# Object resolvers — ProductVariant
# ---------------------------------------------------------------------------

def resolve_ProductVariant_id(args):
    return respond(200, _gid("ProductVariant", args["parent"].get("id", "")))

def resolve_ProductVariant_price(args):
    v = args["parent"]
    return respond(200, _money(v.get("price", "0.00"), "USD"))

def resolve_ProductVariant_inventoryQuantity(args):
    return respond(200, args["parent"].get("inventory_quantity", 0))

def resolve_ProductVariant_sku(args):
    return respond(200, args["parent"].get("sku", ""))

def resolve_ProductVariant_product(args):
    v = args["parent"]
    for p in store_collection("products").list():
        if str(p.get("id", "")) == str(v.get("product_id", "")):
            return respond(200, p)
    return respond(200, None)

# ---------------------------------------------------------------------------
# Object resolvers — Order
# ---------------------------------------------------------------------------

def resolve_Order_id(args):
    return respond(200, _gid("Order", args["parent"]["id"]))

def resolve_Order_createdAt(args):
    return respond(200, args["parent"].get("created_at", ""))

def resolve_Order_updatedAt(args):
    return respond(200, args["parent"].get("updated_at", ""))

def resolve_Order_closedAt(args):
    return respond(200, args["parent"].get("closed_at", None))

def resolve_Order_cancelledAt(args):
    return respond(200, args["parent"].get("cancelled_at", None))

def resolve_Order_cancelReason(args):
    return respond(200, args["parent"].get("cancel_reason", None))

def resolve_Order_totalPrice(args):
    o = args["parent"]
    return respond(200, _money(o.get("total_price", "0.00"), o.get("currency", "USD")))

def resolve_Order_lineItems(args):
    o = args["parent"]
    a = args["args"]
    first = a.get("first")
    if first == None:
        first = 10
    lines = []
    for li in o.get("line_items", []):
        lines.append(_gql_line_item(li))
    return respond(200, _connection(lines, _int_arg(first), a.get("after")))

def resolve_Order_customer(args):
    o = args["parent"]
    stored = o.get("customer", None)
    if stored == None:
        return respond(200, None)
    # Join the full customer record when it exists; otherwise project the
    # denormalized {id, email} stored on the order.
    for c in store_collection("customers").list():
        if str(c.get("id", "")) == str(stored.get("id", "")):
            return respond(200, c)
    return respond(200, stored)

# ---------------------------------------------------------------------------
# Object resolvers — LineItem
# ---------------------------------------------------------------------------

def resolve_LineItem_id(args):
    return respond(200, _gid("LineItem", args["parent"]["id"]))

def resolve_LineItem_discountedTotalPrice(args):
    li = args["parent"]
    return respond(200, _money(li.get("_discounted_total", "0.00"), "USD"))

def resolve_LineItem_fulfillableQuantity(args):
    li = args["parent"]
    remaining = li.get("quantity", 0) - li.get("_fulfilled", 0)
    if remaining < 0:
        remaining = 0
    return respond(200, remaining)

# ---------------------------------------------------------------------------
# Object resolvers — Customer
# ---------------------------------------------------------------------------

def resolve_Customer_id(args):
    return respond(200, _gid("Customer", args["parent"]["id"]))

def resolve_Customer_firstName(args):
    return respond(200, args["parent"].get("first_name", None))

def resolve_Customer_lastName(args):
    return respond(200, args["parent"].get("last_name", None))

def resolve_Customer_displayName(args):
    c = args["parent"]
    name = (c.get("first_name", "") or "") + " " + (c.get("last_name", "") or "")
    name = name.strip()
    if name != "":
        return respond(200, name)
    return respond(200, c.get("email", ""))

def resolve_Customer_phone(args):
    return respond(200, args["parent"].get("phone", None))

def resolve_Customer_ordersCount(args):
    return respond(200, args["parent"].get("orders_count", 0))

def resolve_Customer_totalSpent(args):
    return respond(200, _money(args["parent"].get("total_spent", "0.00"), "USD"))

def resolve_Customer_verifiedEmail(args):
    return respond(200, args["parent"].get("verified_email", True))

def resolve_Customer_createdAt(args):
    return respond(200, args["parent"].get("created_at", ""))

def resolve_Customer_updatedAt(args):
    return respond(200, args["parent"].get("updated_at", ""))

# ---------------------------------------------------------------------------
# Views (shared by webhook payloads + payload returns)
# ---------------------------------------------------------------------------

# _gql_product_view projects a stored product to the GraphQL shape (gid id,
# camelCase fields, variant gids). Internal keys never surface.
def _gql_product_view(p):
    return {
        "id": _gid("Product", p["id"]),
        "title": p.get("title", ""),
        "handle": _slug(p.get("title", "")),
        "description": _strip_html(p.get("body_html", "")),
        "descriptionHtml": p.get("body_html", ""),
        "status": _upper(p.get("status", "active")),
        "vendor": p.get("vendor", ""),
        "productType": p.get("product_type", ""),
        "tags": [],
        "createdAt": p.get("created_at", ""),
        "updatedAt": p.get("updated_at", ""),
        "variants": [],
    }

# _gql_customer_view projects a stored customer to the GraphQL shape.
def _gql_customer_view(c):
    return {
        "id": _gid("Customer", c["id"]),
        "email": c.get("email", ""),
        "displayName": (c.get("first_name", "") or "") + " " + (c.get("last_name", "") or ""),
        "firstName": c.get("first_name", ""),
        "lastName": c.get("last_name", ""),
        "phone": c.get("phone", ""),
        "ordersCount": c.get("orders_count", 0),
        "totalSpent": c.get("total_spent", "0.00"),
        "verifiedEmail": c.get("verified_email", True),
        "createdAt": c.get("created_at", ""),
        "updatedAt": c.get("updated_at", ""),
    }

# _gql_order_view projects a stored order to the GraphQL shape.
def _gql_order_view(o):
    return {
        "id": _gid("Order", o["id"]),
        "name": o.get("name", ""),
        "email": o.get("email", ""),
        "createdAt": o.get("created_at", ""),
        "updatedAt": o.get("updated_at", ""),
        "closedAt": o.get("closed_at", None),
        "cancelledAt": o.get("cancelled_at", None),
        "cancelReason": o.get("cancel_reason", None),
        "totalPrice": o.get("total_price", "0.00"),
        "lineItems": [],
        "customer": o.get("customer", None),
    }

# _gql_line_item projects a stored line item (the per-line _fulfilled
# counter never surfaces; _discounted_total carries the computed total).
def _gql_line_item(li):
    qty = li.get("quantity", 0)
    total = _fmt_money(_cents(li.get("price", "0.00")) * qty)
    return {
        "id": _gid("LineItem", li["id"]),
        "title": li.get("title", ""),
        "quantity": qty,
        "sku": li.get("sku", ""),
        "_discounted_total": total,
        "_fulfilled": li.get("_fulfilled", 0),
    }

# _gql_restock_variant returns quantity to the matching variant's
# inventory (same logic as the REST surface's _restock_variant, kept local
# because handler-script globals do not cross scripts).
def _gql_restock_variant(li, quantity):
    vid = li.get("variant_id", None)
    if vid == None:
        return
    pc = store_collection("products")
    for p in pc.list():
        for v in p.get("variants", []):
            if str(v.get("id", "")) == str(vid):
                v["inventory_quantity"] = v.get("inventory_quantity", 0) + quantity
                pc.update(p["id"], p)
                return

# _lower_status validates + lowercases a ProductStatus enum value for
# storage; returns None (invalid) or the default when the value is absent.
def _lower_status(status, default):
    if status == None:
        return default
    s = str(status).lower()
    if s == "active" or s == "archived" or s == "draft":
        return s
    return None
