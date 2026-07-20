# Financials handler — Workday Financials REST API.
#
# GET /wbs/v40.0/financials/accounts
# -> {data:[...], total, more}

# Shared helpers from lib.star.

def on_accounts(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("accounts")
    docs = col.list()
    return _paginate(req, docs)
