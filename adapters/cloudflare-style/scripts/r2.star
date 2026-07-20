# R2 handlers for the Cloudflare API.
#
# GET   /accounts/{account_id}/r2/buckets -> list R2 buckets
# POST  /accounts/{account_id}/r2/buckets -> create R2 bucket
#
# Stateful: created buckets appear in the buckets list.
# NOTE: R2 list responses use {buckets: [...]} (no result_info pagination).
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_id) are preloaded
# from scripts/lib.star.

# on_list_buckets returns the list of R2 buckets.
def on_list_buckets(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    bc = store_collection("buckets")

    result = []
    for b in bc.list():
        if b.get("account_id", "") == account_id:
            result.append(_bucket_result(b))

    # R2 uses a flat {buckets: [...]} result, not the standard envelope
    return _cf_ok({"buckets": result})

# on_create_bucket creates a new R2 bucket.
def on_create_bucket(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    body = req.get("body")
    if body == None:
        return _cf_err(400, 10004, "Invalid request body.")

    name = body.get("name", "")
    if name == None:
        name = ""
    if name == "":
        return _cf_err(400, 10004, "Missing bucket name.")

    bc = store_collection("buckets")

    # Check for duplicates
    for b in bc.list():
        if b.get("name", "") == name and b.get("account_id", "") == account_id:
            return _cf_err(409, 10004, "Bucket already exists.")

    doc = {
        "name": name,
        "account_id": account_id,
        "creation_date": _iso8601(),
        "location": "ENAM",
    }
    bc.insert(doc)

    return _cf_ok(_bucket_result(doc))

# ====================================================================
# Helpers
# ====================================================================

# _bucket_result returns a clean R2 bucket object for the API response.
def _bucket_result(b):
    return {
        "name": b.get("name", ""),
        "creation_date": b.get("creation_date", _iso8601()),
        "location": b.get("location", "ENAM"),
    }
