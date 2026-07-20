# Payroll handler — Workday Payroll REST API.
#
# GET /wbs/v40.0/payroll/pay_components
# -> {data:[...], total, more}

# Shared helpers from lib.star.

def on_pay_components(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("payComponents")
    docs = col.list()
    return _paginate(req, docs)
