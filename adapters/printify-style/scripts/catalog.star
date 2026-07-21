# Catalog handlers — synthetic blueprint + variant catalog.
#
# GET /v1/catalog/blueprints.json                          (Bearer)
#     -> 200 [ {id, title, description, brand, model}, ... ]
# GET /v1/catalog/blueprints/{blueprint_id}/variants.json  (Bearer)
#     -> 200 [ {id, title, options:{size,color}, price}, ... ]
#
# The catalog is static: two blueprints (T-Shirt id=3, Mug id=71) with
# deterministic variant lists. This supports a client's getVariantId
# placeholder by returning plausible variant data for each blueprint.

# Shared helpers (_bearer, _require_auth) are preloaded from scripts/lib.star.

# --- synthetic catalog data ---

_BLUEPRINTS = [
    {
        "id": 3,
        "title": "Premium T-Shirt",
        "description": "A high-quality cotton t-shirt for custom printing.",
        "brand": "Printify Mock",
        "model": "tshirt-unisex",
        "images": [
            {"src": "https://mock-printify.example/catalog/3.jpg"},
        ],
        "print_areas": [
            {"placement": "front", "width": 1800, "height": 2400},
        ],
    },
    {
        "id": 71,
        "title": "Ceramic Mug",
        "description": "An 11oz ceramic mug for custom designs.",
        "brand": "Printify Mock",
        "model": "mug-11oz",
        "images": [
            {"src": "https://mock-printify.example/catalog/71.jpg"},
        ],
        "print_areas": [
            {"placement": "default", "width": 2000, "height": 1000},
        ],
    },
]

# Variants keyed by blueprint_id.
_VARIANTS = {
    3: [
        {"id": 17835, "title": "S / White", "options": {"size": "S", "color": "White"}, "price": 1200},
        {"id": 17836, "title": "M / White", "options": {"size": "M", "color": "White"}, "price": 1200},
        {"id": 17837, "title": "L / White", "options": {"size": "L", "color": "White"}, "price": 1200},
        {"id": 17838, "title": "S / Black", "options": {"size": "S", "color": "Black"}, "price": 1200},
        {"id": 17839, "title": "M / Black", "options": {"size": "M", "color": "Black"}, "price": 1200},
        {"id": 17840, "title": "L / Black", "options": {"size": "L", "color": "Black"}, "price": 1200},
    ],
    71: [
        {"id": 29145, "title": "11oz / White", "options": {"size": "11oz", "color": "White"}, "price": 900},
        {"id": 29146, "title": "11oz / Black", "options": {"size": "11oz", "color": "Black"}, "price": 900},
    ],
}

# --- handlers ---

# on_list_blueprints returns the full synthetic blueprint catalog.
def on_list_blueprints(req):
    err = _require_auth(req)
    if err != None:
        return err
    return respond(200, {"data": _BLUEPRINTS})

# on_list_variants returns the variants for a given blueprint_id.
def on_list_variants(req):
    err = _require_auth(req)
    if err != None:
        return err

    bp_id = _to_int(req["params"].get("blueprint_id", ""))
    variants = _VARIANTS.get(bp_id)
    if variants == None:
        return respond(404, {"status": 404, "message": "blueprint not found"})
    return respond(200, {"data": variants})
