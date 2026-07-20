# Human Resources handler — Workday HR REST API.
#
# GET /wbs/v40.0/human_resources/positions
# -> {data:[...], total, more}

# Shared helpers from lib.star.

def on_positions(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    col = store_collection("positions")
    docs = col.list()
    return _paginate(req, docs)
