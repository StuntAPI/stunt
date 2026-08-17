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

    # R2 uses a flat {buckets: [...]} result, not the standard envelope.
    # Pagination is via the per_page + cursor query params; the next cursor
    # is returned as a top-level "cursor" field alongside buckets.
    page, next_cursor = _list_page(req, result)
    if page == None:
        return _cf_err(400, 400, "Invalid cursor token")
    res = {"buckets": page}
    if next_cursor != None and next_cursor != "":
        res["cursor"] = next_cursor
    return _cf_ok(res)

# on_create_bucket creates a new R2 bucket.
def on_create_bucket(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    body = req.get("body")
    if body == None:
        return _cf_err(400, 10004, "Invalid request body.")

    name = body.get("name") or ""
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

# on_delete_bucket deletes an R2 bucket by name.
# DELETE /accounts/{account_id}/r2/buckets/{bucket_name}
# Real Cloudflare R2 returns 204 No Content.
def on_delete_bucket(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    bucket_name = req["params"]["bucket_name"]

    bc = store_collection("buckets")
    target = None
    for b in bc.list():
        if b.get("name", "") == bucket_name and b.get("account_id", "") == account_id:
            target = b
            break
    if target == None:
        return _cf_err(404, 10004, "Bucket not found.")

    bc.delete(target.get("id", ""))
    return respond(204, None)

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
