# Test Clocks handlers — deterministic time control.
#
# Real Stripe test clocks (docs.stripe.com/api/test_clocks) freeze time for
# the objects attached to them. stunt's engine clock is read-only, so this
# adapter runs ONE GLOBAL clock: creating or advancing a test clock moves
# _now() (lib.star) for EVERY object the adapter has ever created, not just
# attached ones. Both the short routes (/v1/test_clocks, used by stunt's
# tests) and Stripe's real routes (/v1/test_helpers/test_clocks) hit these
# handlers.
#
# Object shape mirrors the real test_helpers.test_clock: clock_* id,
# frozen_time, status ready|advancing, deletes_after (auto-delete horizon,
# one week), name. The internal_failure status and per-object attachment are
# not simulated. No test_helpers.test_clock.* webhooks are emitted: advancing
# here is synchronous, so the state-change webhooks of the advanced objects
# themselves are the observable signal.
# Shared helpers (_require_auth, _next_id, _not_found, _list_page,
# _newest_first, _created_filters, _created_check, _now, _tc_activate,
# _tc_clear) are in lib.star.

_TC_WEEK = 7 * 24 * 3600  # deletes_after horizon: created + one week

# _tc_public strips the internal soft-delete flag.
def _tc_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# fails to parse arrives as an EMPTY dict via req.body, so req.raw_body is
# the source of truth.
# _tc_missing_param is the real Stripe 400 for a missing required param.
def _tc_missing_param(param):
    return respond(400, {"error": {"type": "invalid_request_error", "message": "Missing required param: " + param + ".", "param": param}})

# _tc_bad_int is the real Stripe 400 for a non-integer param value.
def _tc_bad_int(param, val):
    return respond(400, {"error": {"code": "parameter_invalid_integer", "type": "invalid_request_error", "message": "Invalid integer: " + str(val), "param": param}})

# _tc_get loads a live (non-deleted) clock or None.
def _tc_get(id):
    doc = store_collection("test_clocks").get(id)
    if doc == None:
        return None
    if doc.get("_deleted", False) == True:
        return None
    return doc

# POST /v1/test_clocks — create a test clock at frozen_time (required).
# Creating a clock makes it the active global clock: _now() jumps to
# frozen_time, so objects created afterwards are stamped there.
def on_create_test_clock(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "test_clocks")
    if cached != None:
        return respond(cached["status"], _tc_public(cached["doc"]))

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})
    body = req["body"]
    if body == None:
        body = {}
    ft = body.get("frozen_time", None)
    if ft == None:
        return _tc_missing_param("frozen_time")
    frozen = _num(ft)
    if frozen <= 0:
        return _tc_bad_int("frozen_time", ft)
    now = _now()
    doc = {
        "id": _next_id("clock"),
        "object": "test_helpers.test_clock",
        "created": now,
        "deletes_after": now + _TC_WEEK,
        "frozen_time": frozen,
        "livemode": False,
        "name": body.get("name", None),
        "status": "ready",
        "_deleted": False,
    }
    store_collection("test_clocks").insert(doc)
    _tc_activate(doc["id"], frozen)
    _idempotent_remember(req, "test_clocks", 201, doc["id"])
    return respond(201, _tc_public(doc))

# GET /v1/test_clocks — list clocks (newest first, cursor pagination,
# created filters). Deleted clocks are excluded, like every Stripe list.
def on_list_test_clocks(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("test_clocks").list()
    docs = query_select(docs, [["_deleted", "!=", True]])
    docs = _newest_first(docs)

    page, has_more, e = _list_page(req, docs, "test_clock")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_tc_public(d) for d in page], "has_more": has_more, "url": "/v1/test_clocks"})

# GET /v1/test_clocks/{id} — retrieve a clock.
def on_retrieve_test_clock(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = _tc_get(id)
    if doc == None:
        return _not_found("test_clock", id)
    return respond(200, _tc_public(doc))

# POST /v1/test_clocks/{id}/advance — move the global clock forward.
# Accepts `frozen_time` (the current Stripe param name) and `now` (the
# classic one). The target must be after the clock's current frozen time.
# The stored doc reflects the completed advance (status ready, frozen_time at
# the target); the response carries the documented in-progress status
# "advancing", like real Stripe's asynchronous advance.
def on_advance_test_clock(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, "test_clocks")
    if cached != None:
        out = _tc_public(cached["doc"])
        out["status"] = "advancing"
        return respond(cached["status"], out)

    if _bad_body(req):
        return respond(400, {"error": {"type": "invalid_request_error", "message": "Invalid request body: could not parse as JSON."}})

    id = req["params"]["id"]
    doc = _tc_get(id)
    if doc == None:
        return _not_found("test_clock", id)

    body = req["body"]
    if body == None:
        body = {}
    target = body.get("frozen_time", None)
    param = "frozen_time"
    if target == None:
        target = body.get("now", None)
        param = "now"
    if target == None:
        return _tc_missing_param("frozen_time")
    t = _num(target)
    if t <= 0:
        return _tc_bad_int(param, target)
    if t <= _num(doc.get("frozen_time", 0)):
        return respond(400, {"error": {"code": "test_clock_changing_frozen_time", "type": "invalid_request_error", "message": "The test clock's frozen time cannot be changed to a time in the past, or to the current frozen time."}})

    doc["frozen_time"] = t
    doc["status"] = "ready"
    store_collection("test_clocks").update(id, doc)
    _tc_activate(id, t)
    _idempotent_remember(req, "test_clocks", 200, id)

    out = _tc_public(doc)
    out["status"] = "advancing"
    return respond(200, out)

# DELETE /v1/test_clocks/{id} — delete a clock (soft delete, like real
# Stripe's resource model). Deleting the ACTIVE clock clears the global time
# offset; deleting a stale clock leaves the active one running.
def on_delete_test_clock(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    c = store_collection("test_clocks")
    doc = c.get(id)
    if doc == None or doc.get("_deleted", False) == True:
        return _not_found("test_clock", id)

    doc["_deleted"] = True
    c.update(id, doc)
    _tc_clear(id)
    return respond(200, {"id": id, "object": "test_helpers.test_clock", "deleted": True})
