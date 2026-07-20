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

# on_list_products returns all products as {products:[...]}.
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

    return respond(200, {"products": result})

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
            "id": pid + 1,
            "product_id": pid,
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
        "status": "active",
        "created_at": _now(),
        "updated_at": _now(),
        "variants": built_variants,
    }

    pc = store_collection("products")
    pc.insert(prod)

    # Emit webhook event if any webhooks subscribed to products/create.
    _emit_if_subscribed("products/create", {"id": pid, "title": title})

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

# on_update_product updates a product (PUT).
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

    if input_prod.get("title", None) != None:
        prod["title"] = input_prod["title"]
    if input_prod.get("product_type", None) != None:
        prod["product_type"] = input_prod["product_type"]
    if input_prod.get("body_html", None) != None:
        prod["body_html"] = input_prod["body_html"]
    if input_prod.get("vendor", None) != None:
        prod["vendor"] = input_prod["vendor"]
    prod["updated_at"] = _now()

    pc.update(pid, prod)

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
        "created_at": p.get("created_at", _now()),
        "updated_at": p.get("updated_at", _now()),
        "variants": p.get("variants", []),
    }

# _variant_view normalizes a variant from input.
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

# _emit_if_subscribed emits a webhook event if any webhook subscriptions
# exist for the given topic (fire-and-forget).
def _emit_if_subscribed(topic, payload):
    wc = store_collection("webhooks")
    hooks = wc.list()
    for h in hooks:
        if h.get("topic", "") == topic:
            events_emit(topic, payload)
            return
