# Seaport order handlers — listings, offers, and create offer.
#
# GET  /api/v2/orders/{chain}/{protocol}/listings → { orders: [...] }
# GET  /api/v2/orders/{chain}/{protocol}/offers   → { orders: [...] }
# POST /api/v2/offers  → create offer → { order_hash }
#
# Seaport order shape (EXACT):
#   { order_hash, protocol_address, parameters: {
#       offerer, zone, zone_hash, offer: [{
#         itemType, token, identifierOrCriteria, startAmount, endAmount
#       }], consideration: [{
#         itemType, token, identifierOrCriteria, startAmount, endAmount, recipient
#       }],
#       startTime, endTime, salt, totalOriginalConsiderationItems, counter
#     },
#     signature
#   }
#
# itemType enum: 0=NATIVE, 1=ERC20, 2=ERC721, 3=ERC1155

# Shared helpers (_require_xapikey, _seed, _make_offer, _deterministic_hash,
# _PROTOCOL_ADDRESS, _ITEM_*) are preloaded from scripts/lib.star.

def on_list_listings(req):
    auth_err = _require_xapikey(req)
    if auth_err != None:
        return auth_err

    _seed()

    chain = req["params"].get("chain", "")
    protocol = req["params"].get("protocol", "")

    lc = store_collection("listings")
    result = []
    for listing in lc.list():
        result.append(_strip_id(listing))

    return respond(200, {"orders": result})

def on_list_offers(req):
    auth_err = _require_xapikey(req)
    if auth_err != None:
        return auth_err

    _seed()

    chain = req["params"].get("chain", "")
    protocol = req["params"].get("protocol", "")

    oc = store_collection("offers")
    result = []
    for offer in oc.list():
        result.append(_strip_id(offer))

    return respond(200, {"orders": result})

def on_create_offer(req):
    auth_err = _require_xapikey(req)
    if auth_err != None:
        return auth_err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    # Extract offer parameters.
    criteria = body.get("criteria", {})
    protocol_address = body.get("protocol_address", _PROTOCOL_ADDRESS)
    maker = body.get("maker", "0x0000000000000000000000000000000000000003")

    # Extract NFT info from criteria.
    nft_addr = criteria.get("data", {}).get("token", "0x0000000000000000000000000000000000000100")
    nft_id = criteria.get("data", {}).get("identifier", "1")

    # Extract price.
    consideration = body.get("consideration", {})
    offer_amount = consideration.get("price", "10000000000000000")

    slug = "mock-punks"

    # Build the Seaport offer order.
    offer = _make_offer(slug, nft_addr, nft_id, offer_amount, maker)

    # Store it (STATEFUL — created offers show up in the offers list).
    oc = store_collection("offers")
    oc.insert(offer)

    return respond(200, {
        "order_hash": offer["order_hash"],
        "protocol_address": offer["protocol_address"],
        "chain": offer.get("chain", "ethereum"),
    })

# _strip_id removes the internal "id" field from a stored order doc.
def _strip_id(doc):
    out = {}
    for k in doc:
        if k == "id":
            continue
        out[k] = doc[k]
    return out
