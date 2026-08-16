# Subscription item handlers — the items array of a subscription
# (docs.stripe.com/api/subscription_items) and the metered usage records
# attached to metered items (docs.stripe.com/api/usage_records).
#
# Items are EMBEDDED on the subscription doc (SUBSCRIPTION DOC CONTRACT);
# these endpoints project and mutate them there. GET /v1/subscription_items
# requires the subscription query parameter, like the real API. Deleting the
# last item of a subscription is the real Stripe 400 — a subscription must
# keep at least one item (cancel the subscription instead).
#
# USAGE RECORDS live in the usage_records collection keyed by subscription
# item id, doc {id iid_*, object "usage_record", livemode, quantity,
# subscription_item, timestamp}. action=increment (default) appends a record;
# action=set replaces every record at the same timestamp. At billing
# (scripts/subscriptions.star) the metered invoice line sums the records of
# the billed window — or takes the last record ever when the price sets
# aggregate_usage=last_ever. This simulator also exposes a LIST endpoint at
# GET /v1/subscription_items/{id}/usage_records (newest first); real Stripe
# only offers period summaries (usage_record_summaries) there.
#
# Item endpoints operate on stored state directly (they do not run the
# subscription lifecycle derivation — reads of /v1/subscriptions do).
# Shared helpers (_require_auth, _next_id, _not_found, _get_query,
# _newest_first, _list_page, _idempotent_lookup, _idempotent_remember, _now,
# _num, _signed_emit) are in lib.star.

# arrives as an empty dict for unparseable bodies; req.raw_body is the truth).
def _si_missing(param):
    return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: " + param + ".", "param": param}})

# _si_last_item_error is the real Stripe 400 for deleting the last item on a
# subscription.
def _si_last_item_error():
    return respond(400, {"error": {"type": "invalid_request_error", "message": "Could not delete the last subscription item on a subscription. Cancel the subscription instead using the cancel API.", "param": "subscription"}})

# _si_find locates the subscription doc containing item id. Returns
# (sub_doc, index) or (None, -1).
def _si_find(item_id):
    subs = store_collection("subscriptions").list()
    for i in range(len(subs)):
        items = subs[i].get("items", [])
        if items == None:
            continue
        for j in range(len(items)):
            if items[j].get("id", "") == item_id:
                return subs[i], j
    return None, -1

# GET /v1/subscription_items?subscription= — list a subscription's items,
# projected from the embedded items array (newest subscription first, items
# in creation order, cursor pagination).
def on_list_subscription_items(req):
    err = _require_auth(req)
    if err != None:
        return err

    sub_id = _get_query(req, "subscription")
    if sub_id == "":
        return _si_missing("subscription")
    sub = store_collection("subscriptions").get(sub_id)
    if sub == None:
        return _not_found("subscription", sub_id)

    docs = []
    items = sub.get("items", [])
    if items != None:
        for j in range(len(items)):
            docs.append(items[j])
    page, has_more, e = _list_page(req, docs, "subscription_item")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/subscription_items"})

# POST /v1/subscription_items — add an item to a subscription
# {subscription, price, quantity, tax_rates, proration_behavior}.
# proration_behavior is accepted (always|always_invoice|create_prorations|
# none) and treated as "none": no proration line is generated (documented
# simulator simplification).
def on_create_subscription_item(req):
    err = _require_auth(req)
    if err != None:
        return err

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    body = req["body"]
    if body == None:
        body = {}

    sub_id = body.get("subscription", None)
    if sub_id == None or sub_id == "":
        return _si_missing("subscription")
    sub = store_collection("subscriptions").get(sub_id)
    if sub == None:
        return _not_found("subscription", sub_id)

    price_id = body.get("price", None)
    if price_id == None or price_id == "":
        return _si_missing("price")
    price = store_collection("prices").get(price_id)
    if price == None:
        return _not_found("price", price_id)
    if price.get("recurring", None) == None:
        return respond(400, {"error": {"type": "invalid_request_error", "message": "The price specified is set to `type=one_time` but this field only accepts prices with `type=recurring`.", "param": "price"}})

    qty = _num(body.get("quantity", 1))
    if qty < 1:
        qty = 1
    tr = body.get("tax_rates", [])
    if tr == None:
        tr = []

    item = {
        "id": _next_id("si"),
        "object": "subscription_item",
        "created": _now(),
        "price": price,
        "quantity": qty,
        "subscription": sub_id,
        "tax_rates": tr,
    }
    items = sub.get("items", [])
    if items == None:
        items = []
    items.append(item)
    sub["items"] = items
    store_collection("subscriptions").update(sub_id, sub)
    _signed_emit("customer.subscription.updated", _sub_items_public(sub))
    return respond(201, item)

# _sub_items_public strips the internal "_" keys of a subscription doc so the
# customer.subscription.updated event carries the public object.
def _sub_items_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# POST /v1/subscription_items/{id} — update an item {quantity, metadata,
# tax_rates, proration_behavior (accepted, treated as none)}.
def on_update_subscription_item(req):
    err = _require_auth(req)
    if err != None:
        return err

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    id = req["params"]["id"]
    sub, idx = _si_find(id)
    if sub == None:
        return _not_found("subscription_item", id)

    body = req["body"]
    if body == None:
        body = {}
    item = sub["items"][idx]
    if body.get("quantity", None) != None:
        q = _num(body.get("quantity", 1))
        if q < 1:
            q = 1
        item["quantity"] = q
    if body.get("metadata", None) != None:
        item["metadata"] = body["metadata"]
    if body.get("tax_rates", None) != None:
        tr = body.get("tax_rates", [])
        if tr == None:
            tr = []
        item["tax_rates"] = tr
    sub["items"][idx] = item
    store_collection("subscriptions").update(sub["id"], sub)
    _signed_emit("customer.subscription.updated", _sub_items_public(sub))
    return respond(200, item)

# DELETE /v1/subscription_items/{id} — remove an item from its subscription.
# The last remaining item cannot be deleted (real Stripe 400); cancel the
# subscription instead. Returns the deleted-object shape.
def on_delete_subscription_item(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    sub, idx = _si_find(id)
    if sub == None:
        return _not_found("subscription_item", id)

    items = sub.get("items", [])
    if items == None or len(items) <= 1:
        return _si_last_item_error()
    keep = []
    for j in range(len(items)):
        if j == idx:
            continue
        keep.append(items[j])
    sub["items"] = keep
    store_collection("subscriptions").update(sub["id"], sub)
    _signed_emit("customer.subscription.updated", _sub_items_public(sub))
    return respond(200, {"id": id, "object": "subscription_item", "deleted": True})

# --- Usage records (metered billing) ---

# _siur_item_metered reports whether the item's price bills metered usage.
def _siur_item_metered(item):
    price = item.get("price", None)
    if price == None:
        return False
    rec = price.get("recurring", None)
    if rec == None:
        return False
    return rec.get("usage_type", "licensed") == "metered"

# POST /v1/subscription_items/{id}/usage_records — report usage
# {quantity (required), timestamp (default now), action increment|set}.
# Future timestamps are rejected like the real API; "set" overwrites every
# record at the same timestamp, "increment" (the default) appends.
def on_create_usage_record(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "usage_records")
    if cached != None:
        return respond(cached["status"], cached["doc"])

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    body = req["body"]
    if body == None:
        body = {}

    id = req["params"]["id"]
    sub, idx = _si_find(id)
    if sub == None:
        return _not_found("subscription_item", id)
    item = sub["items"][idx]
    if not _siur_item_metered(item):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "The subscription item's price has usage_type=licensed and does not accept usage records.", "param": "subscription_item"}})

    quantity = body.get("quantity", None)
    if quantity == None:
        return _si_missing("quantity")
    qty = _num(quantity)
    if qty < 0:
        return respond(400, {"error": {"code": "parameter_invalid_integer", "type": "invalid_request_error", "message": "Invalid integer: " + str(quantity), "param": "quantity"}})

    action = body.get("action", "increment")
    if action != "increment" and action != "set":
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid action: must be one of increment or set.", "param": "action"}})

    ts = body.get("timestamp", None)
    if ts == None or ts == "now":
        ts = _now()
    ts_n = _num(ts)
    if ts_n <= 0:
        return respond(400, {"error": {"code": "parameter_invalid_integer", "type": "invalid_request_error", "message": "Invalid integer: " + str(ts), "param": "timestamp"}})
    if ts_n > _now():
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Usage record timestamp must not be in the future.", "param": "timestamp"}})

    if action == "set":
        stale = query_select(store_collection("usage_records").list(), [["subscription_item", "=", id]])
        for i in range(len(stale)):
            if _num(stale[i].get("timestamp", 0)) == ts_n:
                store_collection("usage_records").delete(stale[i]["id"])

    doc = {
        "id": _next_id("iid"),
        "object": "usage_record",
        "livemode": False,
        "quantity": qty,
        "subscription_item": id,
        "timestamp": ts_n,
    }
    store_collection("usage_records").insert(doc)
    _idempotent_remember(req, "usage_records", 201, doc["id"])
    return respond(201, doc)

# GET /v1/subscription_items/{id}/usage_records — list the item's usage
# records, newest first (simulator extension; real Stripe exposes period
# summaries instead).
def on_list_usage_records(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    sub, idx = _si_find(id)
    if sub == None:
        return _not_found("subscription_item", id)

    docs = query_select(store_collection("usage_records").list(), [["subscription_item", "=", id]])
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "usage_record")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": page, "has_more": has_more, "url": "/v1/subscription_items/" + id + "/usage_records"})
