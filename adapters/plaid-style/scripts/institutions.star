# Institution handlers — search and lookup from the institutions store.
#
# POST /institutions/get
#   { count, offset, country_codes, products, options }
#   -> { institutions: [...], total, request_id }
# POST /institutions/get_by_id
#   { institution_id, country_codes }
#   -> { institution: {...}, request_id }

# Shared helpers (_check_auth, _request_id, _inst_public) from lib.star.

# on_list_institutions returns a page of institutions, filtered by the real
# Plaid body params: an institution matches when it supports at least one of
# the requested country_codes and all of the requested products.
def on_list_institutions(req):
    err = _check_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    # Real /institutions/get `count`: 1-500, default 100 (like sync).
    count = body.get("count", 100)
    if count == None or count <= 0:
        count = 100
    if count > 500:
        count = 500
    count = int(count)

    offset = body.get("offset", 0)
    if offset == None or offset < 0:
        offset = 0
    offset = int(offset)

    country_codes = body.get("country_codes", [])
    if country_codes == None:
        country_codes = []
    products = body.get("products", [])
    if products == None:
        products = []

    ic = store_collection("institutions")
    matched = []
    for i in ic.list():
        if not _matches_country(i, country_codes):
            continue
        if not _supports_products(i, products):
            continue
        matched.append(_inst_public(i))

    total = len(matched)
    page = query_select(matched, None, "name", "asc", count, offset, None)

    return respond(200, {
        "institutions": page,
        "total": total,
        "request_id": _request_id(),
    })

# on_get_institution_by_id returns one institution by institution_id.
def on_get_institution_by_id(req):
    err = _check_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    institution_id = body.get("institution_id") or ""
    if institution_id == "":
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_REQUEST",
            "error_code": "MISSING_INSTITUTION_ID",
            "error_message": "institution_id is required",
            "request_id": _request_id(),
        })

    ic = store_collection("institutions")
    doc = ic.get(institution_id)
    if doc == None:
        return respond(400, {
            "display_message": None,
            "error_type": "INVALID_INPUT",
            "error_code": "INVALID_INSTITUTION",
            "error_message": "invalid institution_id",
            "request_id": _request_id(),
        })

    return respond(200, {
        "institution": _inst_public(doc),
        "request_id": _request_id(),
    })

# _matches_country: institution supports at least one requested country (an
# empty request matches everything).
def _matches_country(inst, country_codes):
    if len(country_codes) == 0:
        return True
    have = inst.get("country_codes", [])
    for c in country_codes:
        if c in have:
            return True
    return False

# _supports_products: institution offers every requested product (an empty
# request matches everything).
def _supports_products(inst, products):
    if len(products) == 0:
        return True
    have = inst.get("products", [])
    for p in products:
        if p not in have:
            return False
    return True
