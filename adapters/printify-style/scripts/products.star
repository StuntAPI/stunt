# Product handlers — CRUD for shop products.
#
# GET    /v1/shops/{shop_id}/products.json         (Bearer) -> {data, total, page, limit}
# POST   /v1/shops/{shop_id}/products.json         (Bearer; JSON {title, blueprint_id, print_provider_id, variants, print_areas})
#        -> 200 product object
# GET    /v1/shops/{shop_id}/products/{id}.json    (Bearer) -> product object
# PUT    /v1/shops/{shop_id}/products/{id}.json    (Bearer; JSON partial update) -> product object
#        emits "product.updated" webhook
# DELETE /v1/shops/{shop_id}/products/{id}.json    (Bearer) -> {id, status: "deleted"}
#
# Shared helpers (_bearer, _require_auth, _to_int, _pad4, _product_id)
# are preloaded from scripts/lib.star.

# on_list_products returns a paginated list of products for a shop.
def on_list_products(req):
    err = _require_auth(req)
    if err != None:
        return err

    shop_id = _to_int(req["params"].get("shop_id", ""))
    c = store_collection("products")
    all_docs = c.list()

    # Filter by shop_id.
    shop_docs = []
    for doc in all_docs:
        if doc.get("shop_id") == shop_id:
            shop_docs.append(doc)

    total = len(shop_docs)
    return respond(200, {
        "data": shop_docs,
        "total": total,
        "current_page": 1,
        "per_page": 10,
        "last_page": 1,
        "from": 1 if total > 0 else None,
        "to": total,
    })

# on_create_product creates a new product from a blueprint + variants.
def on_create_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    shop_id = _to_int(req["params"].get("shop_id", ""))
    body = req["body"]
    if body == None:
        body = {}

    title = body.get("title", "Untitled Product")
    description = body.get("description", "")
    blueprint_id = body.get("blueprint_id", 0)
    print_provider_id = body.get("print_provider_id", 1)
    variants = body.get("variants", [])
    print_areas = body.get("print_areas", [])

    seq = store_kv_incr("printify", "product_seq")
    product_id = _product_id(seq)
    ts = 1700000000 + seq

    product = {
        "id": product_id,
        "title": title,
        "description": description,
        "brand": "Printify Mock",
        "blueprint_id": blueprint_id,
        "print_provider_id": print_provider_id,
        "shop_id": shop_id,
        "variants": variants,
        "print_areas": print_areas,
        "created_at": ts,
        "updated_at": ts,
        "is_locked": False,
    }

    c = store_collection("products")
    c.insert(product)

    return respond(200, product)

# on_get_product retrieves a single product by id.
def on_get_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    product_id = _strip_json(req["params"].get("product_id", ""))
    c = store_collection("products")
    doc = c.get(product_id)
    if doc == None:
        return respond(404, {"status": 404, "message": "product not found"})
    return respond(200, doc)

# on_update_product updates a product (partial merge) and emits a webhook.
def on_update_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    product_id = _strip_json(req["params"].get("product_id", ""))
    c = store_collection("products")
    doc = c.get(product_id)
    if doc == None:
        return respond(404, {"status": 404, "message": "product not found"})

    body = req["body"]
    if body == None:
        body = {}

    # Merge fields from the request body.
    for k in body:
        doc[k] = body[k]

    seq = store_kv_incr("printify", "update_seq")
    doc["updated_at"] = 1700001000 + seq
    c.update(product_id, doc)

    # Emit webhook (fire-and-forget).
    events_emit("product.updated", {
        "product_id": product_id,
        "shop_id": doc.get("shop_id"),
        "title": doc.get("title"),
    })

    return respond(200, doc)

# on_delete_product removes a product.
def on_delete_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    product_id = _strip_json(req["params"].get("product_id", ""))
    c = store_collection("products")
    doc = c.get(product_id)
    if doc == None:
        return respond(404, {"status": 404, "message": "product not found"})

    c.delete(product_id)
    return respond(200, {
        "id": product_id,
        "status": "deleted",
    })
