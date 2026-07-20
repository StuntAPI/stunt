# D1 handlers for the Cloudflare API.
#
# GET   /accounts/{account_id}/d1/database             -> list databases
# POST  /accounts/{account_id}/d1/database             -> create database
# POST  /accounts/{account_id}/d1/database/{db}/query  -> execute SQL query
#
# Stateful: created databases appear in the list. The query endpoint
# pattern-matches the SQL and returns seeded rows for common queries.
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_id) are preloaded
# from scripts/lib.star.

# on_list_databases returns the list of D1 databases.
def on_list_databases(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    dc = store_collection("databases")

    result = []
    for d in dc.list():
        if d.get("account_id", "") == account_id:
            result.append(_db_result(d))

    return _cf_ok_with_info(result, len(result))

# on_create_database creates a new D1 database.
def on_create_database(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    body = req.get("body")
    if body == None:
        return _cf_err(400, 10005, "Invalid request body.")

    name = body.get("name", "")
    if name == None:
        name = ""
    if name == "":
        return _cf_err(400, 10005, "Missing database name.")

    dc = store_collection("databases")

    # Check for duplicates
    for d in dc.list():
        if d.get("name", "") == name and d.get("account_id", "") == account_id:
            return _cf_err(409, 10005, "Database already exists.")

    db_uuid = _gen_uuid()
    doc = {
        "uuid": db_uuid,
        "name": name,
        "account_id": account_id,
        "created_at": _iso8601(),
        "file_size": 0,
        "version": "1",
    }
    dc.insert(doc)

    return _cf_ok(_db_result(doc))

# on_query_database executes a SQL query against a D1 database.
# POST /accounts/{account_id}/d1/database/{database_id}/query
# Body: {sql: "SELECT * FROM users LIMIT 10"}
# Response: {result: [{results: [...], success: true, meta: {changes}}]}
#
# We pattern-match the SQL and return seeded rows for common patterns.
def on_query_database(req):
    err = _require_auth(req)
    if err != None:
        return err

    database_id = req["params"]["database_id"]
    account_id = req["params"]["account_id"]

    # Verify database exists
    dc = store_collection("databases")
    db = None
    for d in dc.list():
        if d.get("uuid", "") == database_id and d.get("account_id", "") == account_id:
            db = d
            break
    if db == None:
        return _cf_err(404, 10005, "Database not found.")

    body = req.get("body")
    if body == None:
        return _cf_err(400, 10005, "Missing request body.")

    sql = body.get("sql", "")
    if sql == None:
        sql = ""
    if sql == "":
        return _cf_err(400, 10005, "Missing 'sql' parameter.")

    # Pattern-match the SQL and return seeded rows
    sql_lower = sql.lower()

    # CREATE TABLE — return success, no rows
    if _has_prefix(sql_lower, "create table") or _has_prefix(sql_lower, "create"):
        return _query_result([], 0)

    # INSERT — return success, report 1 change
    if _has_prefix(sql_lower, "insert"):
        return _query_result([], 1)

    # UPDATE — return success, report 1 change
    if _has_prefix(sql_lower, "update"):
        return _query_result([], 1)

    # DELETE — return success, report 1 change
    if _has_prefix(sql_lower, "delete"):
        return _query_result([], 1)

    # SELECT — return seeded rows
    if _has_prefix(sql_lower, "select"):
        return _select_result(sql_lower)

    # Default: return empty
    return _query_result([], 0)

# _select_result returns seeded rows based on the SELECT pattern.
def _select_result(sql_lower):
    # If it mentions "users", return user-like rows
    if _contains(sql_lower, "user"):
        rows = [
            {"id": 1, "email": "alice@stunt.dev", "name": "Alice", "created_at": "2024-01-01T00:00:00Z"},
            {"id": 2, "email": "bob@stunt.dev", "name": "Bob", "created_at": "2024-01-01T00:00:00Z"},
            {"id": 3, "email": "carol@stunt.dev", "name": "Carol", "created_at": "2024-01-01T00:00:00Z"},
        ]
        return _query_result(rows, 0)

    # If it mentions "products" or "orders", return product/order rows
    if _contains(sql_lower, "product") or _contains(sql_lower, "order"):
        rows = [
            {"id": 1, "name": "Widget A", "price": 9.99},
            {"id": 2, "name": "Widget B", "price": 19.99},
            {"id": 3, "name": "Widget C", "price": 29.99},
        ]
        return _query_result(rows, 0)

    # Generic SELECT: return a single seeded row
    rows = [{"id": 1, "name": "stunt-row", "value": 42}]
    return _query_result(rows, 0)

# _query_result returns the D1 query result envelope.
def _query_result(rows, changes):
    return _cf_ok([
        {
            "results": rows,
            "success": True,
            "meta": {
                "changes": changes,
                "duration": 0.12,
                "last_row_id": 1,
                "changed_db": changes > 0,
                "size_after": 4096,
                "rows_read": len(rows),
                "rows_written": changes,
            },
        },
    ])

# ====================================================================
# Helpers
# ====================================================================

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return _find_substr(s, substr) >= 0

# _db_result returns a clean D1 database object for the API response.
def _db_result(d):
    return {
        "uuid": d.get("uuid", ""),
        "name": d.get("name", ""),
        "created_at": d.get("created_at", _iso8601()),
        "file_size": d.get("file_size", 0),
        "version": d.get("version", "1"),
    }
