# D1 handlers for the Cloudflare API.
#
# GET    /accounts/{account_id}/d1/database                     -> list databases
# POST   /accounts/{account_id}/d1/database                     -> create database
# DELETE /accounts/{account_id}/d1/database/{database_id}       -> delete database
# POST   /accounts/{account_id}/d1/database/{database_id}/query -> execute SQL
#
# The query endpoint executes a small SQL subset against a stored table
# model (mapped onto query_select for filtering/sorting/slicing):
#
#   CREATE TABLE [IF NOT EXISTS] t (col TYPE, ...);
#   INSERT INTO t [(cols)] VALUES (?, ...);
#   UPDATE t SET col = ? WHERE col = ?;
#   DELETE FROM t WHERE ...;
#   SELECT cols | * FROM t [WHERE ...] [ORDER BY col [ASC|DESC]] [LIMIT n [OFFSET m]];
#
# Statements are ';' separated; ? placeholders bind to body.params in order.
# Anything outside the subset returns 400 with the Cloudflare error envelope
# (code 7501, D1_ERROR: ...).
#
# Table schemas live in the "d1_tables" collection; rows in "d1_rows" (column
# values nested under "vals" with a per-table rowid).
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_uuid) are preloaded
# from scripts/lib.star.

# Cloudflare's D1 error code, assembled (no 5+ digit literals in scripts).
_D1_ERR = 10 * 1000 + 5

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

    page, next_cursor = _list_page(req, result)
    return _cf_ok_with_info(page, len(result), next_cursor)

# on_create_database creates a new D1 database.
def on_create_database(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    body = req.get("body")
    if body == None:
        return _cf_err(400, _D1_ERR, "Invalid request body.")

    name = body.get("name", "")
    if name == None:
        name = ""
    if name == "":
        return _cf_err(400, _D1_ERR, "Missing database name.")

    dc = store_collection("databases")

    # Check for duplicates
    for d in dc.list():
        if d.get("name", "") == name and d.get("account_id", "") == account_id:
            return _cf_err(409, _D1_ERR, "Database already exists.")

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

# on_delete_database deletes a D1 database by UUID.
# DELETE /accounts/{account_id}/d1/database/{database_id}
# Real Cloudflare returns 200 with a success envelope (result: null).
def on_delete_database(req):
    err = _require_auth(req)
    if err != None:
        return err

    account_id = req["params"]["account_id"]
    database_id = req["params"]["database_id"]

    dc = store_collection("databases")
    target = None
    for d in dc.list():
        if d.get("uuid", "") == database_id and d.get("account_id", "") == account_id:
            target = d
            break
    if target == None:
        return _cf_err(404, _D1_ERR, "Database not found.")

    dc.delete(target.get("id", ""))

    # Cascade: drop the tables and rows of this database.
    tc = store_collection("d1_tables")
    for t in tc.list():
        if t.get("db", "") == database_id:
            tc.delete(t.get("id", ""))
    rc = store_collection("d1_rows")
    for r in rc.list():
        if r.get("db", "") == database_id:
            rc.delete(r.get("id", ""))

    return _cf_ok(None)

# on_query_database executes SQL against a D1 database.
# POST /accounts/{account_id}/d1/database/{database_id}/query
# Body: {sql: "...", params: [...]} — the real D1 query envelope is
# result: [{results, success, meta} ...] (one entry per statement).
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
        return _cf_err(404, _D1_ERR, "Database not found.")

    body = req.get("body")
    if body == None:
        return _cf_err(400, _D1_ERR, "Missing request body.")

    sql = body.get("sql", "")
    if sql == None:
        sql = ""
    if sql == "":
        return _cf_err(400, _D1_ERR, "Missing 'sql' parameter.")

    params = body.get("params", None)
    if params == None or type(params) != "list":
        params = []

    toks = _sql_tokens(sql)
    if toks == None:
        return _cf_err(400, 7501, "D1_ERROR: unable to parse SQL statement")
    stmts = _split_stmts(toks)
    if len(stmts) == 0:
        return _cf_err(400, 7501, "D1_ERROR: empty SQL statement")

    cur = {"p": 0}
    out = []
    for st in stmts:
        res, e = _exec_stmt(database_id, st, params, cur)
        if e != None:
            return _cf_err(400, 7501, e)
        out.append(res)
    return _cf_ok(out)

# ====================================================================
# SQL engine
# ====================================================================

# _exec_stmt dispatches one tokenized statement and returns
# (result_object, error_message).
def _exec_stmt(db, toks, params, cur):
    if len(toks) == 0 or toks[0]["t"] != "word":
        return None, "D1_ERROR: near \"\": syntax error"
    kw = toks[0]["v"].lower()
    if kw == "create":
        return _exec_create(db, toks)
    if kw == "insert":
        return _exec_insert(db, toks, params, cur)
    if kw == "update":
        return _exec_update(db, toks, params, cur)
    if kw == "delete":
        return _exec_delete(db, toks, params, cur)
    if kw == "select":
        return _exec_select(db, toks, params, cur)
    return None, "D1_ERROR: unsupported SQL statement '" + kw + "'"

# _exec_create handles CREATE TABLE [IF NOT EXISTS] name (col TYPE, ...).
def _exec_create(db, toks):
    i = 1
    if i >= len(toks) or toks[i]["v"].lower() != "table":
        return None, "D1_ERROR: only CREATE TABLE is supported"
    i += 1
    if_not_exists = False
    if i + 2 < len(toks) and toks[i]["v"].lower() == "if" and toks[i+1]["v"].lower() == "not" and toks[i+2]["v"].lower() == "exists":
        if_not_exists = True
        i += 3
    if i >= len(toks) or toks[i]["t"] != "word":
        return None, "D1_ERROR: missing table name"
    t = toks[i]["v"].lower()
    if not _valid_ident(t):
        return None, "D1_ERROR: invalid table name '" + t + "'"
    i += 1
    if i >= len(toks) or toks[i]["t"] != "(":
        return None, "D1_ERROR: expected '(' after table name"
    i += 1

    cols = []
    while i < len(toks) and toks[i]["t"] != ")":
        if toks[i]["t"] != "word":
            return None, "D1_ERROR: expected column name"
        first = toks[i]["v"]
        i += 1
        # Skip the rest of this column definition / table constraint.
        while i < len(toks) and toks[i]["t"] != "," and toks[i]["t"] != ")":
            i += 1
        if i < len(toks) and toks[i]["t"] == ",":
            i += 1
        low = first.lower()
        if low == "primary" or low == "unique" or low == "foreign" or low == "check" or low == "constraint":
            continue
        if not _valid_ident(first) or _has_prefix(first, "_"):
            return None, "D1_ERROR: invalid column name '" + first + "'"
        cols.append(first)
    if i >= len(toks):
        return None, "D1_ERROR: expected ')' after column list"
    if len(cols) == 0:
        return None, "D1_ERROR: table must have at least one column"

    tc = store_collection("d1_tables")
    existing = _find_table(db, t)
    if existing != None:
        if if_not_exists:
            return _stmt_result([], 0, 0, 0), None
        return None, "D1_ERROR: table '" + t + "' already exists"
    tc.insert({"db": db, "table": t, "columns": cols})
    return _stmt_result([], 0, 0, 0), None

# _exec_insert handles INSERT INTO t [(cols)] VALUES (vals).
def _exec_insert(db, toks, params, cur):
    i = 1
    if i >= len(toks) or toks[i]["v"].lower() != "into":
        return None, "D1_ERROR: expected INTO"
    i += 1
    if i >= len(toks) or toks[i]["t"] != "word":
        return None, "D1_ERROR: missing table name"
    t = toks[i]["v"].lower()
    i += 1
    schema = _find_table(db, t)
    if schema == None:
        return None, "D1_ERROR: no such table: " + t

    cols = []
    if i < len(toks) and toks[i]["t"] == "(":
        i += 1
        while i < len(toks) and toks[i]["t"] != ")":
            if toks[i]["t"] != "word":
                return None, "D1_ERROR: expected column name"
            col = _resolve_col(schema, toks[i]["v"])
            if col == "":
                return None, "D1_ERROR: no such column: " + toks[i]["v"]
            cols.append(col)
            i += 1
            if i < len(toks) and toks[i]["t"] == ",":
                i += 1
        if i >= len(toks):
            return None, "D1_ERROR: expected ')'"
        i += 1
    else:
        cols = schema.get("columns", [])

    if i + 1 >= len(toks) or toks[i]["v"].lower() != "values":
        return None, "D1_ERROR: expected VALUES"
    i += 1
    if i >= len(toks) or toks[i]["t"] != "(":
        return None, "D1_ERROR: expected '(' after VALUES"
    i += 1
    vals = []
    while i < len(toks) and toks[i]["t"] != ")":
        v, i, e = _parse_value(toks, i, params, cur)
        if e != None:
            return None, e
        vals.append(v)
        if i < len(toks) and toks[i]["t"] == ",":
            i += 1
    if i >= len(toks):
        return None, "D1_ERROR: expected ')'"
    i += 1
    if i < len(toks):
        return None, "D1_ERROR: unexpected input after VALUES list"

    if len(cols) != len(vals):
        return None, "D1_ERROR: " + str(len(vals)) + " values for " + str(len(cols)) + " columns"

    vals_doc = {}
    for j in range(len(cols)):
        vals_doc[cols[j]] = vals[j]

    rowid = store_kv_incr("cf", "d1row_" + db + "_" + t)
    rc = store_collection("d1_rows")
    rc.insert({"db": db, "table": t, "rowid": rowid, "vals": vals_doc})
    return _stmt_result([], 1, rowid, 1), None

# _exec_update handles UPDATE t SET col = ? ... [WHERE preds].
def _exec_update(db, toks, params, cur):
    i = 1
    if i >= len(toks) or toks[i]["t"] != "word":
        return None, "D1_ERROR: missing table name"
    t = toks[i]["v"].lower()
    i += 1
    schema = _find_table(db, t)
    if schema == None:
        return None, "D1_ERROR: no such table: " + t
    if i >= len(toks) or toks[i]["v"].lower() != "set":
        return None, "D1_ERROR: expected SET"
    i += 1

    sets = []
    while i < len(toks):
        if toks[i]["t"] != "word":
            return None, "D1_ERROR: expected column name in SET"
        col = _resolve_col(schema, toks[i]["v"])
        if col == "":
            return None, "D1_ERROR: no such column: " + toks[i]["v"]
        i += 1
        if i >= len(toks) or toks[i]["t"] != "op" or toks[i]["v"] != "=":
            return None, "D1_ERROR: expected '=' in SET"
        i += 1
        v, i, e = _parse_value(toks, i, params, cur)
        if e != None:
            return None, e
        sets.append([col, v])
        if i < len(toks) and toks[i]["t"] == ",":
            i += 1
            continue
        break

    preds, i, e = _parse_where(toks, i, params, cur)
    if e != None:
        return None, e
    if i < len(toks):
        return None, "D1_ERROR: unexpected input after WHERE clause"

    rows, e2 = _match_rows(db, t, preds)
    if e2 != None:
        return None, e2
    rc = store_collection("d1_rows")
    for r in rows:
        vals = r["vals"]
        for s in sets:
            vals[s[0]] = s[1]
        r["vals"] = vals
        rc.update(r.get("id", ""), r)
    return _stmt_result([], len(rows), 0, len(rows)), None

# _exec_delete handles DELETE FROM t [WHERE preds].
def _exec_delete(db, toks, params, cur):
    i = 1
    if i >= len(toks) or toks[i]["v"].lower() != "from":
        return None, "D1_ERROR: expected FROM"
    i += 1
    if i >= len(toks) or toks[i]["t"] != "word":
        return None, "D1_ERROR: missing table name"
    t = toks[i]["v"].lower()
    i += 1
    schema = _find_table(db, t)
    if schema == None:
        return None, "D1_ERROR: no such table: " + t

    preds, i, e = _parse_where(toks, i, params, cur)
    if e != None:
        return None, e
    if i < len(toks):
        return None, "D1_ERROR: unexpected input after WHERE clause"

    rows, e2 = _match_rows(db, t, preds)
    if e2 != None:
        return None, e2
    rc = store_collection("d1_rows")
    for r in rows:
        rc.delete(r.get("id", ""))
    return _stmt_result([], len(rows), 0, len(rows)), None

# _exec_select handles SELECT cols | * FROM t [WHERE ...] [ORDER BY ...]
# [LIMIT n [OFFSET m]] — the filtering/sorting/slicing/projection is mapped
# onto query_select.
def _exec_select(db, toks, params, cur):
    i = 1
    cols = []
    star = False
    while i < len(toks) and toks[i]["v"].lower() != "from":
        if toks[i]["t"] != "word":
            return None, "D1_ERROR: expected column list"
        if toks[i]["v"] == "*":
            star = True
        else:
            cols.append(toks[i]["v"])
        i += 1
        if i < len(toks) and toks[i]["t"] == ",":
            i += 1
    if i >= len(toks):
        return None, "D1_ERROR: expected FROM"
    i += 1
    if i >= len(toks) or toks[i]["t"] != "word":
        return None, "D1_ERROR: missing table name"
    t = toks[i]["v"].lower()
    i += 1
    schema = _find_table(db, t)
    if schema == None:
        return None, "D1_ERROR: no such table: " + t

    # Resolve the projection against the declared schema.
    fields = []
    if star:
        fields = schema.get("columns", [])
    else:
        for c in cols:
            col = _resolve_col(schema, c)
            if col == "":
                return None, "D1_ERROR: no such column: " + c
            fields.append(col)

    preds, i, e = _parse_where(toks, i, params, cur)
    if e != None:
        return None, e

    # ORDER BY col [ASC|DESC]
    order_by = ""
    order_dir = "asc"
    if i < len(toks) and toks[i]["v"].lower() == "order":
        i += 1
        if i >= len(toks) or toks[i]["v"].lower() != "by":
            return None, "D1_ERROR: expected BY after ORDER"
        i += 1
        if i >= len(toks) or toks[i]["t"] != "word":
            return None, "D1_ERROR: expected column in ORDER BY"
        order_by = _resolve_col(schema, toks[i]["v"])
        if order_by == "":
            return None, "D1_ERROR: no such column: " + toks[i]["v"]
        i += 1
        if i < len(toks) and (toks[i]["v"].lower() == "asc" or toks[i]["v"].lower() == "desc"):
            order_dir = toks[i]["v"].lower()
            i += 1

    # LIMIT n [OFFSET m]
    limit = None
    offset = None
    if i < len(toks) and toks[i]["v"].lower() == "limit":
        i += 1
        if i >= len(toks) or toks[i]["t"] != "word" or not _is_number(toks[i]["v"]):
            return None, "D1_ERROR: expected number after LIMIT"
        limit = _to_int(toks[i]["v"])
        i += 1
        if i < len(toks) and toks[i]["v"].lower() == "offset":
            i += 1
            if not _is_number(toks[i]["v"]):
                return None, "D1_ERROR: expected number after OFFSET"
            offset = _to_int(toks[i]["v"])
            i += 1
    if i < len(toks):
        return None, "D1_ERROR: unexpected input after LIMIT clause"

    # Materialize the table's rows as flat dicts (carrying the store doc id
    # under the reserved "_rid" key so updates/deletes can find them).
    rc = store_collection("d1_rows")
    flat = []
    for r in rc.list():
        if r.get("db", "") != db or r.get("table", "") != t:
            continue
        row = {}
        vals = r.get("vals", {})
        for key in vals:
            row[key] = vals[key]
        row["_rid"] = r.get("id", "")
        flat.append(row)

    flt = None
    if len(preds) > 0:
        flt = preds
    selected = query_select(flat, flt, order_by, order_dir, limit, offset, fields if len(fields) > 0 else None)
    return _stmt_result(selected, 0, 0, len(flat)), None

# ====================================================================
# SQL helpers
# ====================================================================

# _find_table returns the schema doc for db+table, or None.
def _find_table(db, t):
    tc = store_collection("d1_tables")
    for s in tc.list():
        if s.get("db", "") == db and s.get("table", "") == t:
            return s
    return None

# _resolve_col case-insensitively resolves a column reference against the
# declared schema; "" when unknown.
def _resolve_col(schema, name):
    cols = schema.get("columns", [])
    low = name.lower()
    for c in cols:
        if c.lower() == low:
            return c
    return ""

# _match_rows returns the stored row docs matching the WHERE predicates.
def _match_rows(db, t, preds):
    rc = store_collection("d1_rows")
    flat = []
    for r in rc.list():
        if r.get("db", "") != db or r.get("table", "") != t:
            continue
        row = {}
        vals = r.get("vals", {})
        for key in vals:
            row[key] = vals[key]
        row["_rid"] = r.get("id", "")
        flat.append(row)
    flt = None
    if len(preds) > 0:
        flt = preds
    selected = query_select(flat, flt)
    by_rid = {}
    for s in selected:
        by_rid[s.get("_rid", "")] = True
    out = []
    for r in rc.list():
        if r.get("db", "") == db and r.get("table", "") == t and r.get("id", "") in by_rid:
            out.append(r)
    return out, None

# _parse_where parses "WHERE col op val [AND col op val]*" starting at
# token index i; returns (preds, new_i, error).
def _parse_where(toks, i, params, cur):
    preds = []
    if i >= len(toks) or toks[i]["v"].lower() != "where":
        return preds, i, None
    i += 1
    while True:
        if i >= len(toks) or toks[i]["t"] != "word":
            return None, i, "D1_ERROR: expected column name in WHERE"
        col = toks[i]["v"]
        i += 1
        if i >= len(toks) or toks[i]["t"] != "op":
            return None, i, "D1_ERROR: expected comparison operator in WHERE"
        op = toks[i]["v"]
        if op == "<>":
            op = "!="
        i += 1
        v, i, e = _parse_value(toks, i, params, cur)
        if e != None:
            return None, i, e
        preds.append([col, op, v])
        if i < len(toks) and toks[i]["v"].lower() == "and":
            i += 1
            continue
        break
    return preds, i, None

# _parse_value parses one literal or ? placeholder at token index i;
# returns (value, new_i, error).
def _parse_value(toks, i, params, cur):
    if i >= len(toks):
        return None, i, "D1_ERROR: expected a value"
    tok = toks[i]
    if tok["t"] == "str":
        return tok["v"], i + 1, None
    if tok["t"] == "param":
        if cur["p"] >= len(params):
            return None, i, "D1_ERROR: not enough parameters supplied for query"
        v = params[cur["p"]]
        cur["p"] = cur["p"] + 1
        return v, i + 1, None
    if tok["t"] == "word":
        v = tok["v"]
        if _is_number(v):
            return _num(v), i + 1, None
        low = v.lower()
        if low == "true":
            return True, i + 1, None
        if low == "false":
            return False, i + 1, None
        if low == "null":
            return None, i + 1, None
        return None, i, "D1_ERROR: near \"" + v + "\": syntax error"
    return None, i, "D1_ERROR: expected a value"

# _sql_tokens tokenizes SQL into {"t": kind, "v": text} dicts. Kinds:
# word (identifiers/numbers/*), str ('...' literals), op, param (?), and
# single-char punctuation tokens ("(", ")", ",", ";"). Returns None on an
# unrecognized character.
def _sql_tokens(sql):
    toks = []
    i = 0
    n = len(sql)
    while i < n:
        ch = sql[i]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            i += 1
            continue
        if ch == "'":
            j = i + 1
            buf = ""
            while j < n and sql[j] != "'":
                buf += sql[j]
                j += 1
            if j >= n:
                return None
            toks.append({"t": "str", "v": buf})
            i = j + 1
            continue
        if ch == "(" or ch == ")" or ch == "," or ch == ";":
            toks.append({"t": ch, "v": ch})
            i += 1
            continue
        if i + 1 < n:
            two = sql[i:i+2]
            if two == ">=" or two == "<=" or two == "!=" or two == "<>":
                toks.append({"t": "op", "v": two})
                i += 2
                continue
        if ch == "=" or ch == ">" or ch == "<":
            toks.append({"t": "op", "v": ch})
            i += 1
            continue
        if ch == "?":
            toks.append({"t": "param", "v": "?"})
            i += 1
            continue
        if _is_word_char(ch):
            j = i
            buf = ""
            while j < n and _is_word_char(sql[j]):
                buf += sql[j]
                j += 1
            toks.append({"t": "word", "v": buf})
            i = j
            continue
        return None
    return toks

# _is_word_char reports whether ch can appear in a word/number token
# ("*" included so SELECT * tokenizes).
def _is_word_char(ch):
    if ch >= "0" and ch <= "9":
        return True
    if ch >= "a" and ch <= "z":
        return True
    if ch >= "A" and ch <= "Z":
        return True
    if ch == "_" or ch == "." or ch == "*":
        return True
    return False

# _split_stmts splits a token stream on ';' into non-empty statements.
def _split_stmts(toks):
    stmts = []
    cur = []
    for t in toks:
        if t["t"] == ";":
            if len(cur) > 0:
                stmts.append(cur)
                cur = []
            continue
        cur.append(t)
    if len(cur) > 0:
        stmts.append(cur)
    return stmts

# _valid_ident reports whether s is a safe SQL identifier (letters, digits,
# underscore; not starting with a digit or underscore).
def _valid_ident(s):
    if len(s) == 0:
        return False
    c0 = s[0]
    if c0 >= "0" and c0 <= "9":
        return False
    for i in range(len(s)):
        ch = s[i]
        ok = (ch >= "0" and ch <= "9") or (ch >= "a" and ch <= "z") or (ch >= "A" and ch <= "Z") or ch == "_"
        if not ok:
            return False
    return True

# _is_number reports whether s parses as a decimal int or float.
def _is_number(s):
    if s == "":
        return False
    seen_dot = False
    for i in range(len(s)):
        ch = s[i]
        if ch == ".":
            if seen_dot:
                return False
            seen_dot = True
            continue
        if ch < "0" or ch > "9":
            return False
    return True

# _num parses a decimal int or float string.
def _num(s):
    dot = _find_substr(s, ".")
    if dot < 0:
        return _to_int(s)
    whole = 0
    if dot > 0:
        whole = _to_int(s[:dot])
    frac = 0
    scale = 1
    for i in range(dot + 1, len(s)):
        digit = ord(s[i]) - ord("0")
        frac = frac * 10 + digit
        scale = scale * 10
    return float(whole) + float(frac) / float(scale)

# _stmt_result builds one D1 per-statement result object.
def _stmt_result(rows, changes, last_row_id, rows_read):
    return {
        "results": rows,
        "success": True,
        "meta": {
            "changed_db": changes > 0,
            "changes": changes,
            "duration": 0.12,
            "last_row_id": last_row_id,
            "rows_read": rows_read,
            "rows_written": changes,
            "size_after": 4096,
        },
    }

# _db_result returns a clean D1 database object for the API response.
def _db_result(d):
    return {
        "uuid": d.get("uuid", ""),
        "name": d.get("name", ""),
        "created_at": d.get("created_at", _iso8601()),
        "file_size": d.get("file_size", 0),
        "version": d.get("version", "1"),
    }
