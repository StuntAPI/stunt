# GraphQL handler — pattern-match common operations.
#
# POST /admin/api/2024-10/graphql.json  {query: "..."} -> {data: {...}}
#
# The Starlark sandbox has no full GraphQL engine, so this handler uses
# substring matching to identify the top-level operation and returns
# real-shaped GraphQL response data. Supported patterns:
#
#   products(first: N)  -> {data:{products:{edges:[{node:{id,title}}]}}}
#   orders(first: N)    -> {data:{orders:{edges:[{node:{id,name}}]}}}
#   customer(id: ...)   -> {data:{customer:{id,email,...}}}
#   shop { ... }        -> {data:{shop:{id,name,myshopifyDomain}}}
#
# Requires X-Shopify-Access-Token.

# Shared helpers (_require_token, _parse_gql_stub, _shopify_err, _seed) are
# preloaded from scripts/lib.star.

def on_graphql(req):
    err = _require_token(req)
    if err != None:
        return err
    _seed()

    body = req["body"]
    if body == None:
        body = {}
    query = body.get("query", "")
    if query == None:
        query = ""

    kind = _parse_gql_stub(query)[0]

    if kind == "products":
        return respond(200, _gql_products())
    if kind == "orders":
        return respond(200, _gql_orders())
    if kind == "customers":
        return respond(200, _gql_customer())
    if kind == "shop":
        return respond(200, _gql_shop())

    # Unknown operation: return an empty data block with no errors.
    return respond(200, {"data": {}})

# _gql_products returns a products GraphQL relay connection.
def _gql_products():
    pc = store_collection("products")
    all_prods = pc.list()
    edges = []
    for p in all_prods:
        edges.append({
            "node": {
                "id": "gid://shopify/Product/" + str(p["id"]),
                "title": p.get("title", ""),
            },
        })
    return {"data": {"products": {"edges": edges, "pageInfo": {"hasNextPage": False, "endCursor": None}}}}

# _gql_orders returns an orders GraphQL relay connection.
def _gql_orders():
    oc = store_collection("orders")
    all_orders = oc.list()
    edges = []
    for o in all_orders:
        edges.append({
            "node": {
                "id": "gid://shopify/Order/" + str(o["id"]),
                "name": o.get("name", ""),
            },
        })
    return {"data": {"orders": {"edges": edges, "pageInfo": {"hasNextPage": False, "endCursor": None}}}}

# _gql_customer returns a single customer (from seeded data).
def _gql_customer():
    cc = store_collection("customers")
    all_customers = cc.list()
    if len(all_customers) == 0:
        return {"data": {"customer": None}}
    c = all_customers[0]
    return {"data": {"customer": {
        "id": "gid://shopify/Customer/" + str(c["id"]),
        "email": c.get("email", ""),
        "firstName": c.get("first_name", ""),
        "lastName": c.get("last_name", ""),
    }}}

# _gql_shop returns shop metadata.
def _gql_shop():
    return {"data": {"shop": {
        "id": "gid://shopify/Shop/1",
        "name": "Stunt Dev Store",
        "myshopifyDomain": "stunt-dev.myshopify.com",
    }}}
