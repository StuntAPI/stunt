# Product handlers — stateful CRUD matching Shopify Admin REST.
#
# GET    /admin/api/2024-10/products.json           -> {products:[...]}
# POST   /admin/api/2024-10/products.json           -> {product:{...}}   (201)
# GET    /admin/api/2024-10/products/{id}.json      -> {product:{...}}
# PUT    /admin/api/2024-10/products/{id}.json      -> {product:{...}}
# DELETE /admin/api/2024-10/products/{id}.json      -> {}   (200, empty envelope)
#
# All endpoints require X-Shopify-Access-Token. The response key is singular
# "product" for single-item responses and plural "products" for lists.

# Shared helpers (_require_token, _shopify_err, _not_found, _next_id,
# _make_product, _seed, _now) are preloaded from scripts/lib.star.

# on_list_products returns products as {products:[...]}. Shopify REST pages
# via the `limit` (page size) and `page_info` (opaque cursor) query params;
# the next cursor round-trips through a 'Link: <url>; rel="next"' header. When
# `limit` is missing paging is disabled and the whole list is returned.
def on_list_products(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    pc = store_collection("products")
    all_prods = pc.list()
    result = []
    for p in all_prods:
        result.append(_product_view(p))

    page, next_cursor = _list_page(req, result)
    headers = None
    link = _next_link(req, next_cursor, _to_int(_get_query(req, "limit")))
    if link != None:
        headers = {"Link": link}
    return respond(200, {"products": page}, headers)

# on_create_product creates a product from the request body.
def on_create_product(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    body = req["body"]
    if body == None:
        body = {}
    input_prod = body.get("product", {})
    if input_prod == None:
        input_prod = {}

    pid = _next_id("products")
    title = input_prod.get("title", "Untitled Product")
    if title == None:
        title = "Untitled Product"
    ptype = input_prod.get("product_type", "")
    if ptype == None:
        ptype = ""

    variants = input_prod.get("variants", [])
    if variants == None:
        variants = []
    built_variants = []
    if len(variants) > 0:
        for v in variants:
            built_variants.append(_variant_view(v, pid))
    else:
        built_variants.append({
            "id": _num_id(pid) + 1,
            "product_id": _num_id(pid),
            "title": "Default Title",
            "price": "0.00",
            "sku": "",
            "inventory_quantity": 0,
        })

    prod = {
        "id": pid,
        "title": title,
        "product_type": ptype,
        "body_html": input_prod.get("body_html", ""),
        "vendor": input_prod.get("vendor", "Stunt Store"),
        "status": _valid_status_or_default(input_prod.get("status", None), "active"),
        "tags": input_prod.get("tags", "") or "",
        "created_at": _now(),
        "updated_at": _now(),
        "variants": built_variants,
    }

    pc = store_collection("products")
    pc.insert(prod)

    # Emit webhook event if any webhooks subscribed to products/create.
    _emit_if_subscribed("products/create", _product_view(prod))

    return respond(201, {"product": _product_view(prod)})

# on_get_product returns a single product by id.
def on_get_product(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    pid = _strip_json(req["params"]["product_id"])
    pc = store_collection("products")
    prod = pc.get(pid)
    if prod == None:
        return _not_found("Product", pid)

    return respond(200, {"product": _product_view(prod)})

# on_update_product updates a product (PUT). Any of title, product_type,
# body_html, vendor, tags, and status may be set; status must be one of
# Shopify's product statuses (active/archived/draft) or the update is a 422.
# A variants array REPLACES the variant set (Shopify semantics): variants
# whose id matches an existing variant are field-merged, id-less variants are
# appended with fresh ids, and an unknown variant id is a 404.
def on_update_product(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    pid = _strip_json(req["params"]["product_id"])
    pc = store_collection("products")
    prod = pc.get(pid)
    if prod == None:
        return _not_found("Product", pid)

    body = req["body"]
    if body == None:
        body = {}
    input_prod = body.get("product", {})
    if input_prod == None:
        input_prod = {}

    status = input_prod.get("status", None)
    if status != None:
        if status != "active" and status != "archived" and status != "draft":
            return respond(422, {"errors": {"status": "Invalid status specified. Status must be active, archived, or draft"}})
        prod["status"] = status
    for key in ["title", "product_type", "body_html", "vendor", "tags"]:
        if input_prod.get(key, None) != None:
            prod[key] = input_prod[key]

    in_variants = input_prod.get("variants", None)
    if in_variants != None and len(in_variants) > 0:
        merged = []
        for v in in_variants:
            vid = v.get("id", None) if v != None else None
            if vid == None:
                merged.append(_variant_from_input(v, pid, _next_id("variants")))
                continue
            match = None
            for ev in prod.get("variants", []):
                if str(ev.get("id", "")) == str(vid):
                    match = ev
            if match == None:
                return _shopify_err(404, "Cannot find variant " + str(vid) + " on product " + str(_num_id(pid)))
            _merge_variant(match, v)
            merged.append(match)
        prod["variants"] = merged

    prod["updated_at"] = _now()
    pc.update(pid, prod)

    _emit_if_subscribed("products/update", _product_view(prod))
    return respond(200, {"product": _product_view(prod)})

# on_delete_product deletes a product. Shopify returns 200 with an empty
# JSON object body {}.
def on_delete_product(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    pid = _strip_json(req["params"]["product_id"])
    pc = store_collection("products")
    prod = pc.get(pid)
    if prod == None:
        return _not_found("Product", pid)

    pc.delete(pid)
    return respond(200, {})

# --- helpers ---

# _product_view returns the public-facing product object.
# Numeric ids are converted from stored strings back to ints for the JSON
# response (Shopify returns numeric ids).
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
        "variants": p.get("variants", []),
    }

# _variant_view normalizes a variant from input (create path). The variant
# gets its own numeric id offset from the product id, matching how Shopify
# seeds the default variant of a new product.
def _variant_view(v, pid):
    if v == None:
        v = {}
    return {
        "id": _num_id(pid) + 1,
        "product_id": _num_id(pid),
        "title": v.get("title", "Default Title"),
        "price": v.get("price", "0.00"),
        "sku": v.get("sku", ""),
        "inventory_quantity": v.get("inventory_quantity", 0),
    }

# _variant_from_input builds a fresh stored variant from input (update path:
# appended id-less variants), keeping the stored string-id convention.
def _variant_from_input(v, pid, vid):
    if v == None:
        v = {}
    return {
        "id": vid,
        "product_id": pid,
        "title": v.get("title", "Default Title") or "Default Title",
        "price": _fmt_money(_cents(v.get("price", "0.00"))),
        "sku": v.get("sku", "") or "",
        "inventory_quantity": v.get("inventory_quantity", 0) or 0,
    }

# _merge_variant merges the writable fields of an input variant into an
# existing stored variant (matched by id).
def _merge_variant(existing, v):
    if v == None:
        return
    for key in ["title", "price", "sku", "barcode", "compare_at_price"]:
        if v.get(key, None) != None:
            existing[key] = v[key]
    inv = v.get("inventory_quantity", None)
    if inv != None:
        if type(inv) == "float":
            inv = int(inv)
        if type(inv) == "int":
            existing["inventory_quantity"] = inv

# _valid_status_or_default validates a product status, returning it when
# valid and the default otherwise (the create path defers hard validation to
# explicit checks only where Shopify documents an error).
def _valid_status_or_default(status, default):
    if status == "active" or status == "archived" or status == "draft":
        return status
    return default
