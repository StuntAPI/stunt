# Product handlers — store product CRUD.
#
# GET  /v2/store/products         (Bearer) -> {data: [...]}
# POST /v2/store/products         (Bearer; JSON {sync_product, sync_variants})
#      -> {id, external_id, name, variants, synced}
# GET  /v2/store/products/{id}    (Bearer) -> {id, sync_product, sync_variants}
#
# Shared helpers (_bearer, _require_auth, _to_int, _next_product_id)
# are preloaded from scripts/lib.star.

# on_list_products returns all store products.
def on_list_products(req):
    err = _require_auth(req)
    if err != None:
        return err

    c = store_collection("products")
    docs = c.list()
    return respond(200, {"data": docs})

# on_create_product creates a new store product.
def on_create_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    sync_product = body.get("sync_product", {})
    if sync_product == None:
        sync_product = {}
    sync_variants = body.get("sync_variants", [])
    if sync_variants == None:
        sync_variants = []

    pid = str(_next_product_id())
    name = sync_product.get("name", "Untitled Product")
    external_id = sync_product.get("external_id", "ext_" + str(pid))

    product = {
        "id": pid,
        "external_id": external_id,
        "name": name,
        "variants": len(sync_variants),
        "synced": len(sync_variants),
        "sync_product": sync_product,
        "sync_variants": sync_variants,
    }

    c = store_collection("products")
    c.insert(product)

    return respond(200, product)

# on_get_product retrieves a single store product by id.
def on_get_product(req):
    err = _require_auth(req)
    if err != None:
        return err

    pid = req["params"].get("product_id", "")
    c = store_collection("products")
    doc = c.get(pid)
    if doc == None:
        return respond(404, {
            "error": {"message": "Product not found", "code": 404},
        })
    return respond(200, doc)
