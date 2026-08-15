# Payment link handlers — Adyen Checkout /paymentLinks.
#
# POST /v68/paymentLinks         → create a hosted payment link (status "active")
# GET  /v68/paymentLinks/{linkId} → retrieve the link, deriving its status
#
# Lifecycle (derive-on-read on GET):
#   active → completed   a payment created with the link's merchant
#                        reference was authorised (see payments.star, which
#                        stamps _paid_at on every matching active link)
#   active → expired     the link's expiresAt (RFC 3339, UTC "Z" format)
#                        is in the past
#
# Each transition is persisted exactly once on first read; later reads return
# the stored terminal status.

# Shared helpers (_require_apikey, _adyen_err, _amt_value) are preloaded
# from scripts/lib.star.

# Default link lifetime when the request omits expiresAt: one day.
_LINK_DEFAULT_TTL = 24 * 3600

# on_create_payment_link creates a hosted payment link.
def on_create_payment_link(req):
    err = _require_apikey(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    amount = body.get("amount", None)
    if amount == None:
        amount = {}
    if _amt_value(amount) <= 0:
        return _adyen_err(422, "710", "amount.value must be greater than zero", "validation")

    reference = body.get("reference", "")
    if reference == None:
        reference = ""
    if reference == "":
        return _adyen_err(422, "711", "reference is missing", "validation")

    merchant_account = body.get("merchantAccount", "TestMerchant")
    reusable = body.get("reusable", False)
    if reusable == None:
        reusable = False

    expires_at = body.get("expiresAt", "")
    if expires_at == None:
        expires_at = ""
    if expires_at == "":
        expires_at = clock.unix_to_rfc3339(clock.now_unix() + _LINK_DEFAULT_TTL)

    link_id = "PL" + str(store_kv_incr("adyen", "pl_seq"))

    doc = {
        "id": link_id,
        "reference": reference,
        "amount": amount,
        "merchantAccount": merchant_account,
        "countryCode": body.get("countryCode", ""),
        "shopperEmail": body.get("shopperEmail", ""),
        "shopperReference": body.get("shopperReference", ""),
        "returnUrl": body.get("returnUrl", ""),
        "reusable": reusable,
        "expiresAt": expires_at,
        "url": _link_url(link_id),
        "status": "active",
        "createdAt": clock.now_rfc3339(),
        "_paid_at": 0,
    }

    lc = store_collection("payment_links")
    lc.insert(doc)

    return respond(200, _link_public(doc))

# on_get_payment_link retrieves a payment link, deriving its current status
# from the stored payment state and the clock before responding.
def on_get_payment_link(req):
    err = _require_apikey(req)
    if err != None:
        return err

    link_id = req["params"]["linkId"]

    lc = store_collection("payment_links")
    doc = lc.get(link_id)
    if doc == None:
        return _adyen_err(404, "191", "Payment link not found", "validation")

    _advance_link(doc, lc)
    return respond(200, _link_public(doc))

# _advance_link derives the link's status on read and persists the
# transition exactly once: completed when a matching payment was authorised,
# expired once expiresAt is past. Terminal statuses are returned as stored.
def _advance_link(doc, lc):
    if doc.get("status", "active") != "active":
        return doc

    if _num(doc.get("_paid_at", 0)) > 0:
        doc["status"] = "completed"
        lc.update(doc["id"], doc)
        return doc

    expires_at = doc.get("expiresAt", "")
    if expires_at != None and expires_at != "" and clock.now_rfc3339() > expires_at:
        doc["status"] = "expired"
        lc.update(doc["id"], doc)
    return doc

# _link_url returns the hosted-checkout URL for a link. The simulator has no
# hosted page, so the URL is a synthetic stable identifier.
def _link_url(link_id):
    return "https://checkout.stunt.local/pay/" + link_id

# _link_public returns the Adyen-shaped payment link object.
def _link_public(doc):
    out = {
        "id": doc["id"],
        "url": doc.get("url", ""),
        "amount": doc.get("amount", {}),
        "reference": doc.get("reference", ""),
        "merchantAccount": doc.get("merchantAccount", ""),
        "reusable": doc.get("reusable", False),
        "expiresAt": doc.get("expiresAt", ""),
        "status": doc.get("status", "active"),
    }
    for k in ["countryCode", "shopperEmail", "shopperReference", "returnUrl"]:
        v = doc.get(k, "")
        if v != None and v != "":
            out[k] = v
    return out
