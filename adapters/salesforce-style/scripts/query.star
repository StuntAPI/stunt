# SOQL Query handler — Salesforce query + queryAll endpoints.
#
# GET /services/data/v60.0/query?q=SELECT+Id,+Name+FROM+Account
# GET /services/data/v60.0/queryAll?q=...
# -> { totalSize, records:[{attributes:{type,url}, Id, Name, ...}], done }
#
# query and queryAll differ exactly as in the real API: query EXCLUDES
# soft-deleted records (IsDeleted, set by DELETE /sobjects/{type}/{id}),
# while queryAll INCLUDES them so clients can read recycle-bin rows
# (typically with WHERE IsDeleted = true).
#
# Results are batched like the real API: at most batchsize records per
# response (default 200, Sforce-Query-Options header, range 200-2000). When
# more remain the response carries done:false and nextRecordsUrl, and
# fetching that URL (GET /query/{queryLocator}) returns the next batch — the
# queryMore round-trip. The locator is an opaque, single-use token whose
# continuation state (original SOQL, absolute offset, queryAll visibility,
# batch size) lives in the KV store.
#
# SOQL parsing: we pattern-match the FROM <Entity> token and the SELECT
# field list. We do NOT implement a full SOQL parser. See lib.star for the
# shared execution pipeline.

# Shared helpers (_require_token, _query_docs, _query_page, _batch_size,
# _sf_error, etc.) from lib.star.

def on_query(req):
    _, err = _require_token(req)
    if err != None:
        return err

    q = req.get("query")
    if q == None:
        q = {}
    soql = q.get("q", "")
    if soql == "":
        return _sf_error(400, "Missing query parameter 'q'", "INVALID_QUERY")

    # queryAll (path-routed) sees recycle-bin rows; plain query does not.
    include_deleted = _contains(req["path"], "queryAll")

    docs, entity, fields, es, em, ec = _query_docs(soql, include_deleted)
    if es != 0:
        return _sf_error(es, em, ec)

    return _query_page(docs, entity, fields, 0, _batch_size(req), include_deleted, soql)

# on_query_more continues a paged query: GET /query/{queryLocator} where the
# locator came from a prior response's nextRecordsUrl. Replaying the original
# SOQL reproduces the same total result set; the stored absolute offset picks
# up where the previous batch stopped.
def on_query_more(req):
    _, err = _require_token(req)
    if err != None:
        return err

    locator = req["params"].get("queryLocator", "")
    raw = None
    if locator != "":
        raw = store_kv_get("salesforce", "qloc_" + locator)
    if raw == None:
        return _sf_error(400, "invalid query locator", "INVALID_QUERY_LOCATOR")

    # Locators are consumed on use (the next batch mints a fresh one).
    store_kv_delete("salesforce", "qloc_" + locator)
    state = json.decode(raw)
    include_deleted = state.get("all_rows", False) == True

    docs, entity, fields, es, em, ec = _query_docs(state.get("soql", ""), include_deleted)
    if es != 0:
        return _sf_error(es, em, ec)

    return _query_page(docs, entity, fields, _to_int(str(state.get("next", 0))), _to_int(str(state.get("batch", _QUERY_BATCH_DEFAULT))), include_deleted, state.get("soql", ""))
