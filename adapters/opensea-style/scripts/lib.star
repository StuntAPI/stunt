# Shared library for opensea-style adapter scripts.
#
# This file is preloaded by stunt before each handler script. Its top-level
# definitions are available to all handlers as predeclared builtins.

# --- auth ---

# _require_xapikey checks for the X-API-KEY header. Returns None if present,
# or an error-response dict if missing.
def _require_xapikey(req):
    headers = req.get("headers", {})
    if headers == None:
        headers = {}
    # Go canonicalizes header keys: X-API-KEY becomes X-Api-Key.
    apikey = headers.get("X-Api-Key", headers.get("X-API-KEY", headers.get("x-api-key", "")))
    if apikey == None or apikey == "":
        return respond(401, {"error": "X-API-KEY header is required"})
    return None

# --- deterministic hashing (consistent with eth-jsonrpc / etherscan adapters) ---

# _deterministic_hash produces a 64-char hex string ("0x" + 64 hex chars)
# from an input string. NOT real keccak256 — a deterministic pseudo-hash.
def _deterministic_hash(input_str):
    lanes = [
        0x811c9dc5,
        0x1000193,
        0xdeadbeef,
        0xcafebabe,
        0x12345678,
        0x9e3779b9,
        0xabcdef01,
        0x0badf00d,
    ]
    for i in range(len(input_str)):
        ch = ord(input_str[i])
        for li in range(len(lanes)):
            lanes[li] = (lanes[li] ^ ch)
            lanes[li] = (lanes[li] * 0x01000193) & 0xffffffff
            lanes[li] = ((lanes[li] << 7) | (lanes[li] >> 25)) & 0xffffffff
    hex_str = ""
    for li in range(len(lanes)):
        hex_str = hex_str + _hex32(lanes[li])
    return "0x" + hex_str

# _hex32 converts a 32-bit int to an 8-char hex string (zero-padded).
def _hex32(n):
    digits = "0123456789abcdef"
    out_chars = []
    for _ in range(8):
        out_chars.append(digits[n % 16])
        n = n // 16
    result = ""
    for i in range(len(out_chars) - 1, -1, -1):
        result = result + out_chars[i]
    return result

# --- constants ---

# Seaport protocol address on Ethereum mainnet (synthetic for mock).
_PROTOCOL_ADDRESS = "0x0000000000000068F116a894984e2DB1123eB395"

# ItemType enum (Seaport):
# 0 = NATIVE (ETH)
# 1 = ERC20
# 2 = ERC721
# 3 = ERC1155
# 4 = ERC721_WITH_CRITERIA
# 5 = ERC1155_WITH_CRITERIA
_ITEM_NATIVE = 0
_ITEM_ERC20 = 1
_ITEM_ERC721 = 2
_ITEM_ERC1155 = 3

# --- seeding ---

# _seed initializes synthetic collections, assets, listings, and offers.
def _seed():
    if store_kv_get("opensea", "seeded") == "yes":
        return
    store_kv_set("opensea", "seeded", "yes")

    # Seed collections.
    cc = store_collection("collections")
    cc.insert({
        "slug": "mock-punks",
        "name": "Mock Punks",
        "description": "A synthetic collection for local testing.",
        "image_url": "https://example.com/mock-punks.png",
        "banner_image_url": "https://example.com/mock-punks-banner.png",
        "external_url": "https://example.com/mock-punks",
        "twitter_username": "mockpunks",
        "discord_url": "https://discord.gg/mock",
        "primary_asset_contracts": [{
            "address": "0x0000000000000000000000000000000000000100",
            "chain": "ethereum",
            "schema_name": "ERC721",
        }],
        "stats": {
            "total_supply": "10000",
            "count": "10000",
            "num_owners": "5000",
            "total_volume": "1000.5",
            "floor_price": "0.05",
        },
    })
    cc.insert({
        "slug": "mock-apes",
        "name": "Mock Apes",
        "description": "Another synthetic collection.",
        "image_url": "https://example.com/mock-apes.png",
        "banner_image_url": "https://example.com/mock-apes-banner.png",
        "external_url": "https://example.com/mock-apes",
        "twitter_username": "mockapes",
        "discord_url": "https://discord.gg/mock2",
        "primary_asset_contracts": [{
            "address": "0x0000000000000000000000000000000000000200",
            "chain": "ethereum",
            "schema_name": "ERC721",
        }],
        "stats": {
            "total_supply": "10000",
            "count": "10000",
            "num_owners": "5000",
            "total_volume": "2000.0",
            "floor_price": "10.5",
        },
    })

    # Seed assets for mock-punks.
    ac = store_collection("assets")
    for i in range(1, 6):
        ac.insert({
            "id": str(i),
            "token_address": "0x0000000000000000000000000000000000000100",
            "token_id": str(i),
            "image_url": "https://example.com/mock-punks/" + str(i) + ".png",
            "name": "Mock Punk #" + str(i),
            "description": "Mock Punk #" + str(i),
            "collection": {
                "slug": "mock-punks",
                "name": "Mock Punks",
            },
            "chain": "ethereum",
        })

    # Seed events.
    ec = store_collection("events")
    ec.insert({
        "event_type": "sale",
        "collection_slug": "mock-punks",
        "asset": {
            "token_address": "0x0000000000000000000000000000000000000100",
            "token_id": "1",
            "name": "Mock Punk #1",
        },
        "from_account": {"address": "0x0000000000000000000000000000000000000001"},
        "to_account": {"address": "0x0000000000000000000000000000000000000002"},
        "quantity": "1",
        "payment": {"token_id": "0x0000000000000000000000000000000000000000", "decimals": "18", "quantity": "50000000000000000"},
        "created_date": "2024-01-01T00:00:00.000Z",
    })

    # Seed a listing (Seaport order shape).
    lc = store_collection("listings")
    lc.insert(_make_listing("mock-punks", "0x0000000000000000000000000000000000000100", "1", "50000000000000000", "0x0000000000000000000000000000000000000001"))

    # Seed an offer.
    oc = store_collection("offers")
    oc.insert(_make_offer("mock-punks", "0x0000000000000000000000000000000000000100", "1", "30000000000000000", "0x0000000000000000000000000000000000000002"))

# --- Seaport order builders ---

# _make_listing builds a Seaport listing order for an NFT.
def _make_listing(slug, nft_addr, nft_id, price_wei, offerer):
    order_hash = _deterministic_hash("listing-" + slug + "-" + nft_id + "-" + offerer)
    return {
        "order_hash": order_hash,
        "protocol_address": _PROTOCOL_ADDRESS,
        "chain": "ethereum",
        "parameters": {
            "offerer": offerer,
            "zone": "0x0000000000000000000000000000000000000000",
            "zone_hash": "0x" + _hex32(0) * 2,
            "offer": [{
                "itemType": _ITEM_ERC721,
                "token": nft_addr,
                "identifierOrCriteria": nft_id,
                "startAmount": "1",
                "endAmount": "1",
            }],
            "consideration": [{
                "itemType": _ITEM_NATIVE,
                "token": "0x0000000000000000000000000000000000000000",
                "identifierOrCriteria": "0",
                "startAmount": price_wei,
                "endAmount": price_wei,
                "recipient": offerer,
            }],
            "startTime": "1700000000",
            "endTime": "1700086400",
            "salt": _deterministic_hash("salt-" + order_hash),
            "totalOriginalConsiderationItems": "1",
            "counter": "0",
        },
        "signature": "0x" + _hex32(0x1234) + _hex32(0x5678) + _hex32(0x9abc) + _hex32(0xdef0) + _hex32(0x1111) + _hex32(0x2222) + _hex32(0x3333) + _hex32(0x4444) + "1b",
        "current_price": price_wei,
        "maker": offerer,
        "taker": None,
        "side": "ask",
    }

# _make_offer builds a Seaport offer order for an NFT.
def _make_offer(slug, nft_addr, nft_id, offer_amount, offerer):
    order_hash = _deterministic_hash("offer-" + slug + "-" + nft_id + "-" + offerer)
    return {
        "order_hash": order_hash,
        "protocol_address": _PROTOCOL_ADDRESS,
        "chain": "ethereum",
        "parameters": {
            "offerer": offerer,
            "zone": "0x0000000000000000000000000000000000000000",
            "zone_hash": "0x" + _hex32(0) * 2,
            "offer": [{
                "itemType": _ITEM_NATIVE,
                "token": "0x0000000000000000000000000000000000000000",
                "identifierOrCriteria": "0",
                "startAmount": offer_amount,
                "endAmount": offer_amount,
            }],
            "consideration": [{
                "itemType": _ITEM_ERC721,
                "token": nft_addr,
                "identifierOrCriteria": nft_id,
                "startAmount": "1",
                "endAmount": "1",
                "recipient": offerer,
            }],
            "startTime": "1700000000",
            "endTime": "1700086400",
            "salt": _deterministic_hash("salt-" + order_hash),
            "totalOriginalConsiderationItems": "1",
            "counter": "0",
        },
        "signature": "0x" + _hex32(0xabcd) + _hex32(0xef01) + _hex32(0x2345) + _hex32(0x6789) + _hex32(0xaaaa) + _hex32(0xbbbb) + _hex32(0xcccc) + _hex32(0xdddd) + "1c",
        "current_price": offer_amount,
        "maker": offerer,
        "taker": None,
        "side": "bid",
    }
