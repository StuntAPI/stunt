# Balance handlers — the account balance object plus the balance-transaction
# ledger the money movements record (lib._bt_record).
#
# GET /v1/balance                docs.stripe.com/api/balance
# GET /v1/balance_transactions   docs.stripe.com/api/balance_transactions/list
# GET /v1/balance_transactions/{id}
#
# The platform balance stays the historical synthetic defaults (tests pin it);
# connected-account balances derive from the KV store (updated by the ledger).
# Both shapes now also carry the real object's connect_reserved and issuing
# arrays (docs.stripe.com/api/balance/object): connect_reserved lists funds
# held for negative connected-account balances (empty here), and issuing is a
# BalanceDetail object with its own available array.
# Shared helpers (_require_auth, _not_found, _stripe_account, _get_balance,
# _list_page, _newest_first, _created_filters, _created_check, _get_query,
# _bt_public) are in lib.star.

# _bal_connect_reserved is the connect_reserved array: entries appear only
# when funds are actually held, so an empty list is the faithful resting value.
def _bal_connect_reserved():
    return []

# _bal_issuing is the issuing BalanceDetail object (its one required field is
# the available array; the simulator holds no Issuing funds).
def _bal_issuing():
    return {"available": [{"amount": 0, "currency": "usd"}]}

# GET /v1/balance — return the account balance.
#
# For Stripe Connect: accepts an optional Stripe-Account header to scope the
# balance to a connected account. When present, returns the tracked per-account
# balance (updated by transfers and payouts; pending stays 0). When absent,
# returns the default platform balance.
def on_get_balance(req):
    err = _require_auth(req)
    if err != None:
        return err

    acct = _stripe_account(req)
    if acct != None:
        # Per-account balance (tracked via KV for Connect).
        bal = _get_balance(acct)
        return respond(200, {
            "object": "balance",
            "available": [
                {"amount": bal, "currency": "usd"},
            ],
            "pending": [
                {"amount": 0, "currency": "usd"},
            ],
            "instant_available": [
                {"amount": 0, "currency": "usd"},
            ],
            "connect_reserved": _bal_connect_reserved(),
            "issuing": _bal_issuing(),
            "livemode": False,
        })

    # Platform balance (default synthetic).
    return respond(200, {
        "object": "balance",
        "available": [
            {"amount": 100000, "currency": "usd"},
        ],
        "pending": [
            {"amount": 50000, "currency": "usd"},
        ],
        "instant_available": [
            {"amount": 25000, "currency": "usd"},
        ],
        "connect_reserved": _bal_connect_reserved(),
        "issuing": _bal_issuing(),
        "livemode": False,
    })

# _bal_apply_filters maps the real Stripe balance-transaction list params
# (created exact/range, currency, payout, source, type — plus simulator-only
# charge/refund/dispute/transfer aliases for source) to query_select clauses.
# Rows are also scoped to the account: with a Stripe-Account header only that
# connected account's rows list, without it only the platform's — like real
# Stripe, which never mixes accounts in one balance history.
def _bal_apply_filters(req, docs):
    f = []

    acct = _stripe_account(req)
    if acct != None:
        f.append(["_account", "=", acct])
    else:
        f.append(["_account", "=", ""])

    source = _get_query(req, "source")
    # The simulator-only aliases map straight onto source (charge=ch_1 is
    # source=ch_1 with an implied type).
    for alias in ["charge", "refund", "dispute", "transfer", "payout"]:
        v = _get_query(req, alias)
        if v != "":
            source = v
    if source != "":
        f.append(["source", "=", source])

    currency = _get_query(req, "currency")
    if currency != "":
        f.append(["currency", "=", currency])
    typ = _get_query(req, "type")
    if typ != "":
        f.append(["type", "=", typ])

    _created_filters(req, f)
    if len(f) == 0:
        return docs
    return query_select(docs, f)

# GET /v1/balance_transactions — list the ledger rows, newest first, with the
# real Stripe list params (created, currency, payout, source, type) and cursor
# paging.
def on_list_balance_transactions(req):
    err = _require_auth(req)
    if err != None:
        return err

    bad = _created_check(req)
    if bad != None:
        return bad

    docs = store_collection("balance_transactions").list()
    docs = _bal_apply_filters(req, docs)
    docs = _newest_first(docs)

    page, has_more, e = _list_page(req, docs, "balance_transaction")
    if e != None:
        return e
    out = []
    for i in range(len(page)):
        out.append(_bt_public(page[i]))
    return respond(200, {"object": "list", "data": out, "has_more": has_more, "url": "/v1/balance_transactions"})

# GET /v1/balance_transactions/{id} — retrieve one ledger row. A row scoped to
# another account (Stripe-Account header) is a 404, like real Stripe.
def on_retrieve_balance_transaction(req):
    err = _require_auth(req)
    if err != None:
        return err

    id = req["params"]["id"]
    doc = store_collection("balance_transactions").get(id)
    if doc == None:
        return _not_found("balance_transaction", id)
    acct = _stripe_account(req)
    if acct != None and doc.get("_account", "") != acct:
        return _not_found("balance_transaction", id)
    return respond(200, _bt_public(doc))
