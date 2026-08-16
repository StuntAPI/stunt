# Connected accounts handlers — Stripe Connect.
#
# Manages Custom/Express/Standard connected accounts stored in the
# connect_accounts collection, plus their sub-resources: external bank
# accounts (ba_*, the "external_accounts" collection) and Express dashboard
# login links.
#
# Capabilities run the real state machine (docs.stripe.com/api/capabilities):
# requesting one on create/update (capabilities[transfers][requested]=true)
# parks it in "pending"; it flips to "active" one day later, derived on read
# via _now() (test-clock aware) and persisted before the account.updated
# emission. The legacy direct-status form ({"transfers": "active"}) is still
# accepted and applies immediately.
#
# Renders follow the real account object (docs.stripe.com/api/accounts/object):
# settings (payouts schedule + branding), business_profile, requirements (full
# shape), external_accounts embedded list, default_currency.
# Emits account.updated, account.external_account.created/deleted.
# Shared helpers (_require_auth, _next_id, _not_found, _now, _num,
# _signed_emit, _list_page, _newest_first, _get_query, _stripe_account) are
# in lib.star.

_CAP_REVIEW_SECS = 24 * 3600  # capability "pending" -> "active" after one day

# _acct_req_shape renders the real requirements object. All keys are always
# present; stored docs (including the seeds) may only carry a subset.
def _acct_req_shape(req):
    if req == None:
        req = {}
    return {
        "alternatives": req.get("alternatives", []),
        "current_deadline": req.get("current_deadline", None),
        "currently_due": req.get("currently_due", []),
        "disabled_reason": req.get("disabled_reason", None),
        "errors": req.get("errors", []),
        "eventually_due": req.get("eventually_due", []),
        "past_due": req.get("past_due", []),
        "pending_verification": req.get("pending_verification", []),
    }

# _acct_default_reqs is the empty-requirements baseline for a fresh account.
def _acct_default_reqs():
    return _acct_req_shape(None)

# _acct_settings renders settings.payouts (schedule delay_days/interval,
# statement_descriptor) and settings.branding per the real account object.
def _acct_settings(doc):
    s = doc.get("settings", None)
    if s == None:
        s = {}
    p = s.get("payouts", None)
    if p == None:
        p = {}
    sch = p.get("schedule", None)
    if sch == None:
        sch = {}
    b = s.get("branding", None)
    if b == None:
        b = {}
    return {
        "branding": {
            "icon": b.get("icon", None),
            "logo": b.get("logo", None),
            "primary_color": b.get("primary_color", None),
            "secondary_color": b.get("secondary_color", None),
        },
        "payouts": {
            "debit_negative_balances": p.get("debit_negative_balances", True),
            "schedule": {
                "delay_days": _num(sch.get("delay_days", 2)),
                "interval": sch.get("interval", "daily"),
            },
            "statement_descriptor": p.get("statement_descriptor", None),
        },
    }

# _acct_bp renders business_profile with every documented key present.
def _acct_bp(doc):
    bp = doc.get("business_profile", None)
    if bp == None:
        bp = {}
    return {
        "mcc": bp.get("mcc", None),
        "name": bp.get("name", None),
        "product_description": bp.get("product_description", None),
        "support_address": bp.get("support_address", None),
        "support_email": bp.get("support_email", None),
        "support_phone": bp.get("support_phone", None),
        "support_url": bp.get("support_url", None),
        "url": bp.get("url", None),
    }

# _acct_merge_settings deep-merges a request `settings` object into the stored
# doc (three explicit levels: settings -> payouts/branding -> schedule — no
# recursion in Starlark).
def _acct_merge_settings(doc, s):
    if s == None or type(s) != "dict":
        return
    cur = doc.get("settings", None)
    if cur == None:
        cur = {}
    for k in s:
        v = s[k]
        c = cur.get(k, None)
        if type(v) == "dict" and type(c) == "dict":
            for sk in v:
                sv = v[sk]
                sc = c.get(sk, None)
                if type(sv) == "dict" and type(sc) == "dict":
                    for ssk in sv:
                        sc[ssk] = sv[ssk]
                    c[sk] = sc
                else:
                    c[sk] = sv
            cur[k] = c
        else:
            cur[k] = v
    doc["settings"] = cur

# _acct_merge_bp merges a request business_profile (one nested level) into the
# stored doc.
def _acct_merge_bp(doc, bp):
    if bp == None or type(bp) != "dict":
        return
    cur = doc.get("business_profile", None)
    if cur == None:
        cur = {}
    for k in bp:
        v = bp[k]
        c = cur.get(k, None)
        if type(v) == "dict" and type(c) == "dict":
            for sk in v:
                c[sk] = v[sk]
            cur[k] = c
        else:
            cur[k] = v
    doc["business_profile"] = cur

# _acct_apply_caps folds a request capabilities hash into the doc. Two forms:
#   {"transfers": "active"}            legacy direct status (applies now)
#   {"transfers": {"requested": true}} the real request form -> "pending"
# Requested-at timestamps are tracked in the internal _caps map so the
# pending -> active derivation knows when the review window ends.
def _acct_apply_caps(doc, body_caps):
    if body_caps == None or type(body_caps) != "dict":
        return
    caps = doc.get("capabilities", None)
    if caps == None:
        caps = {}
    meta = doc.get("_caps", None)
    if meta == None:
        meta = {}
    for name in body_caps:
        val = body_caps[name]
        if type(val) == "string":
            caps[name] = val
        elif type(val) == "dict":
            m = meta.get(name, None)
            if m == None:
                m = {"requested": False, "requested_at": 0}
            if val.get("requested", False) == True and m.get("requested", False) != True:
                m["requested"] = True
                m["requested_at"] = _now()
                if caps.get(name, None) != "active":
                    caps[name] = "pending"
            meta[name] = m
    doc["capabilities"] = caps
    doc["_caps"] = meta

# _acct_sync_flags derives the enablement booleans from active capabilities:
# card_payments -> charges_enabled, transfers -> payouts_enabled (forward
# only; stored true flags from earlier docs are never reset).
def _acct_sync_flags(doc):
    caps = doc.get("capabilities", None)
    if caps == None:
        return
    if caps.get("card_payments", None) == "active":
        doc["charges_enabled"] = True
    if caps.get("transfers", None) == "active":
        doc["payouts_enabled"] = True

# _acct_advance derives capability activations from the clock: every
# "pending" capability whose one-day review window has elapsed becomes
# "active". The doc is persisted BEFORE the account.updated emission, and the
# transition fires exactly once (status no longer "pending" afterwards).
def _acct_advance(doc):
    caps = doc.get("capabilities", None)
    meta = doc.get("_caps", None)
    if caps == None or meta == None:
        return doc
    now = _now()
    changed = False
    # Snapshot the names first: Starlark forbids inserting into a dict while
    # iterating it, and the loop below mutates caps.
    names = []
    for name in caps:
        names.append(name)
    for i in range(len(names)):
        name = names[i]
        if caps.get(name, None) != "pending":
            continue
        m = meta.get(name, None)
        if m == None:
            continue
        rat = _num(m.get("requested_at", 0))
        if rat > 0 and now >= rat + _CAP_REVIEW_SECS:
            caps[name] = "active"
            changed = True
    if not changed:
        return doc
    _acct_sync_flags(doc)
    store_collection("connect_accounts").update(doc["id"], doc)
    _signed_emit("account.updated", _acct_public(doc))
    return doc

# _ea_public strips internal "_" keys from a stored bank-account doc.
def _ea_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    return out

# _acct_ea_docs returns the external bank-account docs attached to an account.
def _acct_ea_docs(acct_id):
    docs = store_collection("external_accounts").list()
    return query_select(docs, [["account", "=", acct_id]])

# _acct_ea_embedded renders the external_accounts list object embedded on the
# account (docs.stripe.com/api/accounts/object: object/data/has_more/
# total_count/url).
def _acct_ea_embedded(acct_id):
    eas = _acct_ea_docs(acct_id)
    return {
        "object": "list",
        "data": [_ea_public(d) for d in eas],
        "has_more": False,
        "total_count": len(eas),
        "url": "/v1/accounts/" + acct_id + "/external_accounts",
    }

# _acct_public renders the full account shape. Stored docs (including the
# seed fixtures) carry only a subset of fields; every documented key is filled
# with its default here.
def _acct_public(doc):
    caps = doc.get("capabilities", None)
    if caps == None:
        caps = {}
    return {
        "id": doc["id"],
        "object": "account",
        "business_profile": _acct_bp(doc),
        "business_type": doc.get("business_type", None),
        "capabilities": caps,
        "charges_enabled": doc.get("charges_enabled", False) == True,
        "country": doc.get("country", "US"),
        "created": _num(doc.get("created", 0)),
        "default_currency": doc.get("default_currency", "usd"),
        "details_submitted": doc.get("details_submitted", False) == True,
        "email": doc.get("email", None),
        "external_accounts": _acct_ea_embedded(doc["id"]),
        "login_links": {
            "object": "list",
            "data": [],
            "has_more": False,
            "total_count": 0,
            "url": "/v1/accounts/" + doc["id"] + "/login_links",
        },
        "metadata": doc.get("metadata", {}),
        "payouts_enabled": doc.get("payouts_enabled", False) == True,
        "requirements": _acct_req_shape(doc.get("requirements", None)),
        "settings": _acct_settings(doc),
        "tos_acceptance": doc.get("tos_acceptance", {"date": None, "ip": None, "user_agent": None}),
        "type": doc.get("type", None),
    }

# POST /v1/accounts — create a connected account.
def on_create_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    acct_id = _next_id("acct")
    acct_type = body.get("type", None)
    if acct_type == None or acct_type == "":
        acct_type = "express"
    dc = body.get("default_currency", None)
    if dc == None or dc == "":
        dc = "usd"

    doc = {
        "id": acct_id,
        "object": "account",
        "type": acct_type,
        "country": body.get("country", "US"),
        "default_currency": dc,
        "email": body.get("email", None),
        "business_type": body.get("business_type", None),
        "capabilities": {},
        "_caps": {},
        "details_submitted": False,
        "charges_enabled": False,
        "payouts_enabled": False,
        "requirements": _acct_default_reqs(),
        "settings": {},
        "business_profile": {},
        "metadata": body.get("metadata", {}),
        "created": _now(),
    }

    # Express and Custom accounts are platform-controlled, so Stripe
    # auto-requests both core capabilities at creation (they sit "pending"
    # until the review window elapses). Standard accounts request nothing.
    if acct_type == "express" or acct_type == "custom":
        _acct_apply_caps(doc, {"transfers": {"requested": True}, "card_payments": {"requested": True}})
    _acct_apply_caps(doc, body.get("capabilities", None))
    _acct_merge_settings(doc, body.get("settings", None))
    _acct_merge_bp(doc, body.get("business_profile", None))
    _acct_sync_flags(doc)

    store_collection("connect_accounts").insert(doc)

    # Emit webhook event (fire-and-forget: errors do not break account creation).
    _signed_emit("account.updated", _acct_public(doc))

    return respond(201, _acct_public(doc))

# GET /v1/accounts/{id} — retrieve a single connected account (capability
# activations are derived first, so polls agree with the webhook timeline).
def on_retrieve_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("connect_accounts").get(id)
    if doc == None:
        return _not_found("account", id)
    return respond(200, _acct_public(_acct_advance(doc)))

# POST /v1/accounts/{id} — update a connected account (capabilities,
# settings, business_profile, metadata, ...). Only documented top-level
# params are merged.
def on_update_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("connect_accounts").get(id)
    if doc == None:
        return _not_found("account", id)

    body = req["body"]
    if body != None:
        for k in ["email", "business_type", "default_currency", "country", "metadata", "tos_acceptance"]:
            v = body.get(k, None)
            if v != None:
                doc[k] = v
        _acct_merge_settings(doc, body.get("settings", None))
        _acct_merge_bp(doc, body.get("business_profile", None))
        _acct_apply_caps(doc, body.get("capabilities", None))
        _acct_sync_flags(doc)

    store_collection("connect_accounts").update(id, doc)

    # Emit webhook event (fire-and-forget).
    _signed_emit("account.updated", _acct_public(doc))

    return respond(200, _acct_public(doc))

# GET /v1/accounts — list connected accounts (newest first, cursor
# pagination, created filters).
def on_list_accounts(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("connect_accounts").list()
    docs = [_acct_advance(d) for d in docs]
    docs = _apply_account_filters(req, docs)
    docs = _newest_first(docs)

    page, has_more, err2 = _list_page(req, docs, "account")
    if err2 != None:
        return err2
    return respond(200, {"object": "list", "data": [_acct_public(d) for d in page], "has_more": has_more, "url": "/v1/accounts"})

# _apply_account_filters maps the real Stripe account-list query params
# (created exact/range) to query_select clauses, applied before paging like
# the real API.
def _apply_account_filters(req, docs):
    f = []
    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# ============================================================================
# EXTERNAL ACCOUNTS (bank accounts attached to a connected account)
# ============================================================================

# _ea_last4 extracts the last four characters of an account number without
# ever storing the raw number (plain indexing/slicing math, no negative
# indices).
def _ea_last4(number):
    n = str(number)
    ln = len(n)
    if ln <= 4:
        return n
    return n[ln - 4:ln]

# POST /v1/accounts/{id}/external_accounts — attach a bank account.
# Accepts the real forms: external_account as a bank_account hash, the
# deprecated bank_account alias, or a bank-account token id. Only last4,
# fingerprint and routing number are persisted — never the account number.
def on_create_external_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    acct = store_collection("connect_accounts").get(id)
    if acct == None:
        return _not_found("account", id)

    body = req["body"]
    if body == None:
        body = {}
    ea = body.get("external_account", None)
    if ea == None:
        ea = body.get("bank_account", None)

    fields = None
    if ea != None and type(ea) == "dict":
        fields = ea
    elif ea != None and type(ea) == "string":
        tok = store_collection("tokens").get(ea)
        if tok == None:
            return _not_found("token", ea)
        ba = tok.get("bank_account", None)
        if ba == None or type(ba) != "dict":
            return respond(400, {"error": {"message": "The token is not a bank account token.", "param": "external_account", "type": "invalid_request_error"}})
        fields = ba
    if fields == None:
        return respond(400, {"error": {"message": "Missing required param: external_account.", "param": "external_account", "type": "invalid_request_error"}})

    number = fields.get("account_number", None)
    if number == None or number == "":
        return respond(400, {"error": {"message": "Missing required param: external_account[account_number].", "param": "external_account[account_number]", "type": "invalid_request_error"}})

    currency = fields.get("currency", None)
    if currency == None or currency == "":
        currency = acct.get("default_currency", "usd")

    # The first external account for a currency becomes its default; an
    # explicit default_for_currency=true demotes the previous default.
    want_default = fields.get("default_for_currency", False) == True
    same_currency = query_select(_acct_ea_docs(id), [["currency", "=", currency]])
    if not want_default and len(same_currency) == 0:
        want_default = True
    if want_default:
        for i in range(len(same_currency)):
            other = same_currency[i]
            if other.get("default_for_currency", False) == True:
                other["default_for_currency"] = False
                store_collection("external_accounts").update(other["id"], other)

    doc = {
        "id": _next_id("ba"),
        "object": "bank_account",
        "account": id,
        "account_holder_name": fields.get("account_holder_name", None),
        "account_holder_type": fields.get("account_holder_type", None),
        "account_type": None,
        "available_payout_methods": ["standard"],
        "bank_name": "STRIPE TEST BANK",
        "country": fields.get("country", acct.get("country", "US")),
        "currency": currency,
        "default_for_currency": want_default,
        "fingerprint": "fp_" + str(store_kv_incr("stripe", "fp_seq")),
        "last4": _ea_last4(number),
        "metadata": fields.get("metadata", {}),
        "routing_number": fields.get("routing_number", None),
        "status": "new",
        "created": _now(),
    }
    store_collection("external_accounts").insert(doc)
    _signed_emit("account.external_account.created", _ea_public(doc))
    return respond(201, _ea_public(doc))

# GET /v1/accounts/{id}/external_accounts — list a connected account's bank
# accounts (newest first, cursor pagination).
def on_list_external_accounts(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    if store_collection("connect_accounts").get(id) == None:
        return _not_found("account", id)

    docs = _newest_first(_acct_ea_docs(id))
    page, has_more, err2 = _list_page(req, docs, "external_account")
    if err2 != None:
        return err2
    return respond(200, {"object": "list", "data": [_ea_public(d) for d in page], "has_more": has_more, "url": "/v1/accounts/" + id + "/external_accounts"})

# GET /v1/accounts/{id}/external_accounts/{ea_id} — retrieve one bank account.
def on_retrieve_external_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    ea_id = req["params"]["ea_id"]
    if store_collection("connect_accounts").get(id) == None:
        return _not_found("account", id)
    doc = store_collection("external_accounts").get(ea_id)
    if doc == None or doc.get("account", None) != id:
        return _not_found("external_account", ea_id)
    return respond(200, _ea_public(doc))

# DELETE /v1/accounts/{id}/external_accounts/{ea_id} — detach a bank account.
# The real API refuses to delete a default external account while its
# currency is the account's default currency or another external account
# shares the currency (you must re-default another one first).
def on_delete_external_account(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    ea_id = req["params"]["ea_id"]
    acct = store_collection("connect_accounts").get(id)
    if acct == None:
        return _not_found("account", id)
    doc = store_collection("external_accounts").get(ea_id)
    if doc == None or doc.get("account", None) != id:
        return _not_found("external_account", ea_id)

    if doc.get("default_for_currency", False) == True:
        others = query_select(_acct_ea_docs(id), [["currency", "=", doc.get("currency", "usd")]])
        siblings = 0
        for i in range(len(others)):
            if others[i].get("id", None) != ea_id:
                siblings = siblings + 1
        if doc.get("currency", "usd") == acct.get("default_currency", "usd") or siblings > 0:
            return respond(400, {"error": {"message": "Cannot delete the default external account. Set default_for_currency on another external account with the same currency first.", "param": "default_for_currency", "type": "invalid_request_error"}})

    store_collection("external_accounts").delete(ea_id)
    _signed_emit("account.external_account.deleted", _ea_public(doc))
    return respond(200, {"id": ea_id, "object": "bank_account", "deleted": True})

# ============================================================================
# LOGIN LINKS (Express dashboard)
# ============================================================================

# POST /v1/accounts/{id}/login_links — create a single-use Express dashboard
# login link (docs.stripe.com/connect/express-accounts). Standard accounts
# manage their own Stripe login, so the real API refuses them.
def on_create_login_link(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    acct = store_collection("connect_accounts").get(id)
    if acct == None:
        return _not_found("account", id)
    if acct.get("type", "express") == "standard":
        return respond(400, {"error": {"message": "Login links cannot be created for standard accounts.", "param": "account", "type": "invalid_request_error"}})

    seq = store_kv_incr("stripe", "login_link_seq")
    url = "https://connect.stunt.local/" + id + "/" + str(seq)
    return respond(200, {"object": "login_link", "created": _now(), "url": url})
