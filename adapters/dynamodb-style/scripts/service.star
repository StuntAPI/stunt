# DynamoDB-style service handlers.
#
# POST / dispatches on the X-Amz-Target header, whose value is the
# _DDB_TARGET_PREFIX from lib.star ("DynamoDB_" + API date) + <Operation>:
#   CreateTable        -> create a table
#   DescribeTable      -> table metadata (live counts)
#   ListTables         -> paginated table names
#   DeleteTable        -> drop a table and its items
#   PutItem            -> insert/replace an item
#   GetItem            -> fetch by full key
#   DeleteItem         -> remove by full key
#   UpdateItem         -> SET/REMOVE/ADD (upsert)
#   Query              -> pk equality + optional sk range
#   Scan               -> full table + FilterExpression
#   BatchGetItem       -> up to 25 keys per table
#   BatchWriteItem     -> put/delete mix, 25 per table
#
# Items are stored as DynamoDB typed attribute values verbatim. Expressions
# (Key/Condition/Filter/Update/Projection) support the documented subset —
# see the adapter README. Shared helpers (_require_auth, _validation_err,
# the expression parser/evaluator, ... ) are preloaded from scripts/lib.star.

_RETURN_VALUES_ITEM = ["NONE", "ALL_OLD", "ALL_NEW", "UPDATED_OLD", "UPDATED_NEW"]

_LEGACY_PARAMS = ["AttributesToGet", "Expected", "AttributeUpdates", "KeyConditions", "ScanFilter", "QueryFilter", "ConditionalOperator", "ComparisonOperator", "AttributeValueList"]

# on_service_api is the single dispatch handler for the whole API.
def on_service_api(req):
    err = _require_auth(req)
    if err != None:
        return err

    target = req["headers"].get("X-Amz-Target", "")
    if target == None or target == "":
        return _validation_err("Missing X-Amz-Target header")
    if not _has_prefix(target, _DDB_TARGET_PREFIX):
        return _validation_err("Unsupported X-Amz-Target: " + target + "; expected the " + _DDB_TARGET_PREFIX + "<Operation> form")
    op = target[len(_DDB_TARGET_PREFIX):]
    body = _json_body(req)

    lerr = _reject_legacy(body)
    if lerr != None:
        return lerr

    if op == "CreateTable":
        return _op_create_table(body)
    if op == "DescribeTable":
        return _op_describe_table(body)
    if op == "ListTables":
        return _op_list_tables(body)
    if op == "DeleteTable":
        return _op_delete_table(body)
    if op == "PutItem":
        return _op_put_item(body)
    if op == "GetItem":
        return _op_get_item(body)
    if op == "DeleteItem":
        return _op_delete_item(body)
    if op == "UpdateItem":
        return _op_update_item(body)
    if op == "Query":
        return _op_query(body)
    if op == "Scan":
        return _op_scan(body)
    if op == "BatchGetItem":
        return _op_batch_get_item(body)
    if op == "BatchWriteItem":
        return _op_batch_write_item(body)

    return _ddb_err("UnknownOperationException", "Operation not supported: " + op)

def _reject_legacy(body):
    # The pre-expression legacy parameters are rejected so clients get a
    # clear nudge to the ExpressionAttribute* forms this simulator speaks.
    for p in _LEGACY_PARAMS:
        if p in body:
            return _validation_err("The parameter " + p + " is not supported by this simulator; use the ExpressionAttribute* parameters")
    return None

# _require_table resolves TableName to a table doc. not_found_msg differs
# per operation (real DynamoDB wording).
def _require_table(body, not_found_msg):
    name = body.get("TableName", "")
    if type(name) != "string" or name == "":
        return [None, _validation_err("TableName must be specified")]
    doc = _find_table(name)
    if doc == None:
        return [None, _resource_not_found(not_found_msg)]
    return [doc, None]

# ====================================================================
# Table operations
# ====================================================================

def _valid_table_name(name):
    for i in range(len(name)):
        ch = name[i]
        ok = (ch >= "a" and ch <= "z") or (ch >= "A" and ch <= "Z") or (ch >= "0" and ch <= "9") or ch == "_" or ch == "-" or ch == "."
        if not ok:
            return False
    return True

def _op_create_table(body):
    name = body.get("TableName", "")
    if type(name) != "string" or len(name) < 3:
        return _validation_err("TableName must be at least 3 characters long")
    if len(name) > 255:
        return _validation_err("TableName must be at most 255 characters long")
    if not _valid_table_name(name):
        return _validation_err("TableName must only contain a-z, A-Z, 0-9, '_', '-', and '.'")

    if "GlobalSecondaryIndexes" in body or "LocalSecondaryIndexes" in body:
        return _validation_err("Secondary indexes are not supported by this simulator")

    ks = body.get("KeySchema", None)
    if ks == None or type(ks) != "list" or len(ks) == 0:
        return _validation_err("One or more parameter values were invalid: KeySchema must have at least one element")
    if len(ks) > 2:
        return _validation_err("One or more parameter values were invalid: KeySchema may only contain HASH and RANGE elements")
    hash_attr = ""
    range_attr = ""
    for e in ks:
        if type(e) != "dict":
            return _validation_err("Invalid KeySchema element")
        an = e.get("AttributeName", "")
        kt = e.get("KeyType", "")
        if an == "" or kt == "":
            return _validation_err("Both AttributeName and KeyType must be specified in KeySchema")
        if kt == "HASH" and hash_attr == "":
            hash_attr = an
        elif kt == "RANGE" and range_attr == "":
            range_attr = an
        else:
            return _validation_err("One or more parameter values were invalid: KeySchema may contain at most one HASH and one RANGE element")
    if hash_attr == "":
        return _validation_err("One or more parameter values were invalid: KeySchema does not contain a HASH element")
    if range_attr == hash_attr:
        return _validation_err("One or more parameter values were invalid: Duplicate attribute " + hash_attr + " in KeySchema")

    defs = body.get("AttributeDefinitions", None)
    if defs == None or type(defs) != "list":
        return _validation_err("AttributeDefinitions must be specified")
    types = {}
    for d in defs:
        if type(d) != "dict":
            return _validation_err("Invalid AttributeDefinitions element")
        an = d.get("AttributeName", "")
        at = d.get("AttributeType", "")
        if an == "" or at == "":
            return _validation_err("Both AttributeName and AttributeType must be specified in AttributeDefinitions")
        if at not in _KEY_TYPES:
            return _validation_err("One or more parameter values were invalid: AttributeType must be one of S, N, B")
        if an in types:
            return _validation_err("Duplicate attribute " + an + " in AttributeDefinitions")
        types[an] = at
    want = 1
    if range_attr != "":
        want = 2
    if len(types) != want:
        return _validation_err("One or more parameter values were invalid: Number of attributes in KeySchema does not exactly match number of attributes defined in AttributeDefinitions")
    for a in types:
        if a != hash_attr and a != range_attr:
            return _validation_err("One or more parameter values were invalid: AttributeDefinitions may only contain key attributes in this simulator")
    for a in [hash_attr, range_attr]:
        if a != "" and a not in types:
            return _validation_err("One or more parameter values were invalid: Some key attributes are not defined in AttributeDefinitions. Keys: " + a + ",")

    bm = body.get("BillingMode", "PROVISIONED")
    if bm == None or bm == "":
        bm = "PROVISIONED"
    provisioned = None
    if bm != "PAY_PER_REQUEST":
        pt = body.get("ProvisionedThroughput", None)
        if pt == None or type(pt) != "dict":
            return _validation_err("One or more parameter values were invalid: ProvisionedThroughput must be specified when BillingMode is PROVISIONED")
        rcu = _as_int(pt.get("ReadCapacityUnits", 0))
        wcu = _as_int(pt.get("WriteCapacityUnits", 0))
        if rcu <= 0 or wcu <= 0:
            return _validation_err("One or more parameter values were invalid: ReadCapacityUnits and WriteCapacityUnits must be positive")
        provisioned = {"read": str(rcu), "write": str(wcu)}

    if _find_table(name) != None:
        return _ddb_err("ResourceInUseException", "Table already exists: " + name)

    range_type = ""
    if range_attr != "":
        range_type = types[range_attr]
    doc = {
        "id": name,
        "name": name,
        "hashAttr": hash_attr,
        "hashType": types[hash_attr],
        "rangeAttr": range_attr,
        "rangeType": range_type,
        "createdUnix": str(clock.now_unix()),
        "status": "ACTIVE",
        "billingMode": bm,
        "provisioned": provisioned,
    }
    _tables().insert(doc)
    return respond(200, {"TableDescription": _table_description(doc)})

def _op_describe_table(body):
    doc, err = _require_table(body, "Table not found: " + str(body.get("TableName", "")))
    if err != None:
        return err
    return respond(200, {"Table": _table_description(doc)})

def _op_list_tables(body):
    names = []
    for d in _tables().list():
        if d.get("name", "") != "":
            names.append(d["name"])
    names = _sig_sort_strings(names)
    excl = body.get("ExclusiveStartTableName", "")
    offset = 0
    if excl != None and excl != "":
        found = len(names)
        for i in range(len(names)):
            if names[i] > excl:
                found = i
                break
        offset = found
    limit = body.get("Limit", None)
    if limit == None:
        page, nxt = paginate(names, None, str(offset))
    else:
        n = _as_int(limit)
        if n <= 0:
            return _validation_err("The Limit parameter must be a positive integer")
        page, nxt = paginate(names, n, str(offset))
    out = {"TableNames": page}
    if nxt != None and len(page) > 0:
        out["LastEvaluatedTableName"] = page[len(page) - 1]
    return respond(200, out)

def _op_delete_table(body):
    doc, err = _require_table(body, "Table not found: " + str(body.get("TableName", "")))
    if err != None:
        return err
    name = doc.get("name", "")
    desc = _table_description(doc)
    ic = _items()
    for d in _table_items(name):
        ic.delete(d.get("id", ""))
    _tables().delete(name)
    return respond(200, {"TableDescription": desc})

# ====================================================================
# Item operations
# ====================================================================

# _validate_item_for_table checks an Item against the table schema and
# returns [keyAttrMap, ""] or [None, errorMessage].
def _validate_item_for_table(tdoc, item):
    m = _validate_item(item)
    if m != "":
        return [None, m]
    h = tdoc.get("hashAttr", "")
    r = tdoc.get("rangeAttr", "")
    if h not in item:
        return [None, "One or more parameter values were invalid: Missing the key " + h + " in the item"]
    if r != "" and r not in item:
        return [None, "One or more parameter values were invalid: Missing the key " + r + " in the item"]
    if _attr_type(item[h]) != tdoc.get("hashType", "S"):
        return [None, "One or more parameter values were invalid: Type mismatch for key " + h]
    if r != "" and _attr_type(item[r]) != tdoc.get("rangeType", "S"):
        return [None, "One or more parameter values were invalid: Type mismatch for key " + r]
    return [_item_key(tdoc, item), ""]

# _check_condition evaluates ConditionExpression against the current item
# image (None when absent). Returns [True, None] when it holds, or
# [False, ConditionalCheckFailedException response].
def _check_condition(body, existing):
    expr = body.get("ConditionExpression", None)
    if expr == None or expr == "":
        return [True, None]
    terms, err = _parse_condition(expr, "ConditionExpression", _expr_names(body), _expr_values(body))
    if err != None:
        return [False, err]
    image = {}
    if existing != None:
        image = existing
    if _cond_matches(terms, image):
        return [True, None]
    if body.get("ReturnValuesOnConditionCheckFailure", "") == "ALL_OLD" and existing != None:
        return [False, _conditional_check_failed(existing)]
    return [False, _conditional_check_failed(None)]

def _return_values_ok(body):
    rv = body.get("ReturnValues", "NONE")
    if rv == None:
        rv = "NONE"
    if rv not in _RETURN_VALUES_ITEM:
        return [None, _validation_err("Invalid ReturnValues: " + str(rv))]
    return [rv, None]

def _op_put_item(body):
    tdoc, err = _require_table(body, "Cannot do operations on a non-existent table")
    if err != None:
        return err
    name = tdoc.get("name", "")
    item = body.get("Item", None)
    key, kerr = _validate_item_for_table(tdoc, item)
    if kerr != "":
        return _validation_err(kerr)
    kid, kiderr = _key_id(tdoc, key)
    if kiderr != "":
        return _validation_err(kiderr)
    old_doc = _items().get(kid)
    old_attrs = None
    if old_doc != None:
        old_attrs = old_doc.get("attrs", {})
    ok, cerr = _check_condition(body, old_attrs)
    if not ok:
        return cerr
    rv, rverr = _return_values_ok(body)
    if rverr != None:
        return rverr
    _upsert_item(kid, name, key, item)
    out = {}
    if rv == "ALL_OLD" and old_attrs != None:
        out["Attributes"] = old_attrs
    cap = _capacity(body, name)
    if cap != None:
        out["ConsumedCapacity"] = cap
    return respond(200, out)

def _op_get_item(body):
    tdoc, err = _require_table(body, "Cannot do operations on a non-existent table")
    if err != None:
        return err
    kid, kiderr = _key_id(tdoc, body.get("Key", None))
    if kiderr != "":
        return _validation_err(kiderr)
    # Projection validated before the miss early-return: a bad
    # ProjectionExpression must 400 even when the item does not exist.
    fields, ferr = _projection_fields(body)
    if ferr != None:
        return ferr
    out = {}
    doc = _items().get(kid)
    if doc != None:
        out["Item"] = _project(doc.get("attrs", {}), fields)
    cap = _capacity(body, tdoc.get("name", ""))
    if cap != None:
        out["ConsumedCapacity"] = cap
    return respond(200, out)

def _op_delete_item(body):
    tdoc, err = _require_table(body, "Cannot do operations on a non-existent table")
    if err != None:
        return err
    name = tdoc.get("name", "")
    kid, kiderr = _key_id(tdoc, body.get("Key", None))
    if kiderr != "":
        return _validation_err(kiderr)
    doc = _items().get(kid)
    old_attrs = None
    if doc != None:
        old_attrs = doc.get("attrs", {})
    ok, cerr = _check_condition(body, old_attrs)
    if not ok:
        return cerr
    rv, rverr = _return_values_ok(body)
    if rverr != None:
        return rverr
    if doc != None:
        _items().delete(kid)
    out = {}
    if rv == "ALL_OLD" and old_attrs != None:
        out["Attributes"] = old_attrs
    cap = _capacity(body, name)
    if cap != None:
        out["ConsumedCapacity"] = cap
    return respond(200, out)

def _op_update_item(body):
    tdoc, err = _require_table(body, "Cannot do operations on a non-existent table")
    if err != None:
        return err
    name = tdoc.get("name", "")
    key = body.get("Key", None)
    kid, kiderr = _key_id(tdoc, key)
    if kiderr != "":
        return _validation_err(kiderr)
    expr = body.get("UpdateExpression", None)
    if expr == None or expr == "":
        return _validation_err("Invalid UpdateExpression: The expression can not be empty")
    actions, uerr = _parse_update(expr, _expr_names(body), _expr_values(body))
    if uerr != None:
        return uerr
    doc = _items().get(kid)
    old_attrs = {}
    if doc != None:
        old_attrs = doc.get("attrs", {})
    ok, cerr = _check_condition(body, old_attrs)
    if not ok:
        return cerr
    rv, rverr = _return_values_ok(body)
    if rverr != None:
        return rverr
    new_attrs, touched, amerr = _apply_update(old_attrs, actions)
    if amerr != "":
        return _validation_err(amerr)
    # UpdateItem upserts: the key attributes always land in the item.
    for k in key:
        new_attrs[k] = key[k]
    _upsert_item(kid, name, key, new_attrs)
    out = {}
    if rv == "ALL_OLD" and len(old_attrs) > 0:
        out["Attributes"] = old_attrs
    elif rv == "ALL_NEW":
        out["Attributes"] = new_attrs
    elif rv == "UPDATED_OLD":
        out["Attributes"] = _subset(old_attrs, touched)
    elif rv == "UPDATED_NEW":
        out["Attributes"] = _subset(new_attrs, touched)
    cap = _capacity(body, name)
    if cap != None:
        out["ConsumedCapacity"] = cap
    return respond(200, out)

def _subset(attrs, touched):
    out = {}
    for k in touched:
        if k in attrs:
            out[k] = attrs[k]
    return out

# ====================================================================
# Query / Scan
# ====================================================================

def _sorted_by_key(docs, attr, descending):
    pairs = []
    for d in docs:
        pairs.append([d.get("k", {}).get(attr, None), d])
    i = 1
    while i < len(pairs):
        v = pairs[i]
        j = i - 1
        while j >= 0 and _typed_cmp(pairs[j][0], v[0]) > 0:
            pairs[j + 1] = pairs[j]
            j = j - 1
        pairs[j + 1] = v
        i = i + 1
    out = []
    for p in pairs:
        out.append(p[1])
    if not descending:
        return out
    rev = []
    n = len(out)
    for x in range(n):
        rev.append(out[n - 1 - x])
    return rev

def _sorted_by_id(docs):
    pairs = []
    for d in docs:
        pairs.append([d.get("id", ""), d])
    i = 1
    while i < len(pairs):
        v = pairs[i]
        j = i - 1
        while j >= 0 and pairs[j][0] > v[0]:
            pairs[j + 1] = pairs[j]
            j = j - 1
        pairs[j + 1] = v
        i = i + 1
    out = []
    for p in pairs:
        out.append(p[1])
    return out

# _after_start drops items at or before the ExclusiveStartKey in the
# current scan direction (strictly-after resume, like real DynamoDB).
def _after_start(ordered, sort_attr, start, descending):
    sv = start.get(sort_attr, None)
    if sv == None:
        return []
    out = []
    for d in ordered:
        v = d.get("k", {}).get(sort_attr, None)
        if v == None:
            continue
        c = _typed_cmp(v, sv)
        if descending:
            if c < 0:
                out.append(d)
        else:
            if c > 0:
                out.append(d)
    return out

# _read_page applies Limit via the paginate builtin. Returns
# [page, nextCursor, None] or [None, None, errorResponse].
def _read_page(ordered, body):
    limit = body.get("Limit", None)
    if limit == None:
        page, nxt = paginate(ordered, None, None)
        return [page, nxt, None]
    n = _as_int(limit)
    if n <= 0:
        return [None, None, _validation_err("The Limit parameter must be a positive integer")]
    page, nxt = paginate(ordered, n, None)
    return [page, nxt, None]

def _op_query(body):
    tdoc, err = _require_table(body, "Cannot do operations on a non-existent table")
    if err != None:
        return err
    if "IndexName" in body:
        return _validation_err("Secondary indexes are not supported by this simulator")
    name = tdoc.get("name", "")
    h = tdoc.get("hashAttr", "")
    r = tdoc.get("rangeAttr", "")

    expr = body.get("KeyConditionExpression", None)
    if expr == None or expr == "":
        return _validation_err("Either the KeyConditions or KeyConditionExpression parameter must be specified in the request.")
    names = _expr_names(body)
    values = _expr_values(body)
    parsed, perr = _parse_key_condition(expr, names, values)
    if perr != None:
        return perr
    if parsed["pk"] != h:
        return _validation_err("Query condition missed key schema element: " + h)
    if parsed["sk"] != "":
        if r == "" or parsed["sk"] != r:
            return _validation_err("Query condition missed key schema element: " + r)

    terms = None
    fexpr = body.get("FilterExpression", None)
    if fexpr != None and fexpr != "":
        terms, ferr = _parse_condition(fexpr, "FilterExpression", names, values)
        if ferr != None:
            return ferr

    select = body.get("Select", "")
    if select != "" and select != "ALL_ATTRIBUTES" and select != "ALL_PROJECTED_ATTRIBUTES" and select != "SPECIFIC_ATTRIBUTES" and select != "COUNT":
        return _validation_err("Invalid Select: " + str(select))
    if select == "SPECIFIC_ATTRIBUTES" and (body.get("ProjectionExpression", None) == None or body.get("ProjectionExpression", "") == ""):
        return _validation_err("Select type SPECIFIC_ATTRIBUTES requires ProjectionExpression")
    # Projection validated up front so an empty result page still 400s on
    # a bad expression.
    fields, pferr = _projection_fields(body)
    if pferr != None:
        return pferr

    keyed = []
    for d in _table_items(name):
        attrs = d.get("attrs", {})
        hv = attrs.get(h, None)
        if hv == None:
            continue
        if _typed_cmp(hv, parsed["pkv"]) != 0:
            continue
        if parsed["sk"] != "":
            sv = attrs.get(parsed["sk"], None)
            if sv == None or not _sk_matches(parsed, sv):
                continue
        keyed.append(d)
    scanned = len(keyed)
    matched = keyed
    if terms != None:
        matched = []
        for d in keyed:
            if _cond_matches(terms, d.get("attrs", {})):
                matched.append(d)

    sort_attr = r
    if sort_attr == "":
        sort_attr = h
    fwd = body.get("ScanIndexForward", None)
    descending = False
    if fwd == False:
        descending = True
    ordered = _sorted_by_key(matched, sort_attr, descending)

    start = body.get("ExclusiveStartKey", None)
    if start != None and type(start) == "dict":
        # The starting key must be a full, schema-valid key — real DynamoDB
        # rejects a start key missing the sort attribute instead of
        # quietly returning an empty page.
        sid, siderr = _key_id(tdoc, start)
        if siderr != "":
            return _validation_err("The provided starting key is invalid: " + siderr)
        sh = start.get(h, None)
        if sh == None or _typed_cmp(sh, parsed["pkv"]) != 0:
            return _validation_err("The provided starting key is invalid: Invalid hash key provided")
        ordered = _after_start(ordered, sort_attr, start, descending)

    page, nxt, perr = _read_page(ordered, body)
    if perr != None:
        return perr

    out = {"Count": len(page), "ScannedCount": scanned}
    if select != "COUNT":
        items = []
        for d in page:
            items.append(_project(d.get("attrs", {}), fields))
        out["Items"] = items
    if nxt != None and len(page) > 0:
        out["LastEvaluatedKey"] = page[len(page) - 1].get("k", {})
    cap = _capacity(body, name)
    if cap != None:
        out["ConsumedCapacity"] = cap
    return respond(200, out)

def _op_scan(body):
    tdoc, err = _require_table(body, "Cannot do operations on a non-existent table")
    if err != None:
        return err
    if "IndexName" in body:
        return _validation_err("Secondary indexes are not supported by this simulator")
    name = tdoc.get("name", "")
    names = _expr_names(body)
    values = _expr_values(body)
    terms = None
    fexpr = body.get("FilterExpression", None)
    if fexpr != None and fexpr != "":
        terms, ferr = _parse_condition(fexpr, "FilterExpression", names, values)
        if ferr != None:
            return ferr
    select = body.get("Select", "")
    if select != "" and select != "ALL_ATTRIBUTES" and select != "ALL_PROJECTED_ATTRIBUTES" and select != "SPECIFIC_ATTRIBUTES" and select != "COUNT":
        return _validation_err("Invalid Select: " + str(select))
    # Projection validated up front so an empty result page still 400s on
    # a bad expression.
    fields, pferr = _projection_fields(body)
    if pferr != None:
        return pferr

    docs = _table_items(name)
    scanned = len(docs)
    matched = docs
    if terms != None:
        matched = []
        for d in docs:
            if _cond_matches(terms, d.get("attrs", {})):
                matched.append(d)
    ordered = _sorted_by_id(matched)

    start = body.get("ExclusiveStartKey", None)
    if start != None and type(start) == "dict":
        sid, siderr = _key_id(tdoc, start)
        if siderr != "":
            return _validation_err(siderr)
        after = []
        for d in ordered:
            if d.get("id", "") > sid:
                after.append(d)
        ordered = after

    page, nxt, perr = _read_page(ordered, body)
    if perr != None:
        return perr

    out = {"Count": len(page), "ScannedCount": scanned}
    if select != "COUNT":
        items = []
        for d in page:
            items.append(_project(d.get("attrs", {}), fields))
        out["Items"] = items
    if nxt != None and len(page) > 0:
        out["LastEvaluatedKey"] = page[len(page) - 1].get("k", {})
    cap = _capacity(body, name)
    if cap != None:
        out["ConsumedCapacity"] = cap
    return respond(200, out)

# ====================================================================
# Batch operations
# ====================================================================

def _op_batch_get_item(body):
    ri = body.get("RequestItems", None)
    if ri == None or type(ri) != "dict" or len(ri) == 0:
        return _validation_err("RequestItems must not be empty")
    responses = {}
    caps = []
    for tname in ri:
        spec = ri[tname]
        if spec == None or type(spec) != "dict":
            return _validation_err("RequestItems[" + str(tname) + "] must be an object")
        tdoc = _find_table(tname)
        if tdoc == None:
            return _resource_not_found("Cannot do operations on a non-existent table")
        keys = spec.get("Keys", None)
        if keys == None or type(keys) != "list":
            return _validation_err("RequestItems[" + str(tname) + "].Keys must be a list")
        if len(keys) > _MAX_BATCH:
            return _validation_err("Too many items requested for the BatchGetItemList call")
        # Per-table projection validated before any key lookups (a bad
        # expression must 400 even when nothing is found).
        fields, pferr = _projection_fields(spec)
        if pferr != None:
            return pferr
        seen = {}
        got = []
        for k in keys:
            kid, kerr = _key_id(tdoc, k)
            if kerr != "":
                return _validation_err(kerr)
            if kid in seen:
                continue
            seen[kid] = True
            doc = _items().get(kid)
            if doc == None:
                continue
            got.append(_project(doc.get("attrs", {}), fields))
        responses[tname] = got
        cap = _capacity(body, tname)
        if cap != None:
            caps.append(cap)
    out = {"Responses": responses, "UnprocessedKeys": {}}
    if len(caps) > 0:
        out["ConsumedCapacity"] = caps
    return respond(200, out)

def _op_batch_write_item(body):
    ri = body.get("RequestItems", None)
    if ri == None or type(ri) != "dict" or len(ri) == 0:
        return _validation_err("RequestItems must not be empty")
    caps = []
    plan = []
    # Pass 1 validates EVERY request (all tables, both kinds) before
    # anything is applied — real DynamoDB validates the whole batch, so a
    # later 400 must not leave earlier writes committed.
    for tname in ri:
        reqs = ri[tname]
        if reqs == None or type(reqs) != "list":
            return _validation_err("RequestItems[" + str(tname) + "] must be a list of write requests")
        if len(reqs) > _MAX_BATCH:
            return _validation_err("Too many items requested for the BatchWriteItemList call")
        tdoc = _find_table(tname)
        if tdoc == None:
            return _resource_not_found("Cannot do operations on a non-existent table")
        for wr in reqs:
            if type(wr) != "dict" or len(wr) != 1:
                return _validation_err("One or more parameter values were invalid: each write request must be exactly one of PutRequest/DeleteRequest")
            if "PutRequest" in wr:
                pr = wr["PutRequest"]
                if pr == None or type(pr) != "dict":
                    return _validation_err("Invalid PutRequest")
                item = pr.get("Item", None)
                key, kerr = _validate_item_for_table(tdoc, item)
                if kerr != "":
                    return _validation_err(kerr)
                kid, kiderr = _key_id(tdoc, key)
                if kiderr != "":
                    return _validation_err(kiderr)
                plan.append(["put", kid, tname, key, item])
            elif "DeleteRequest" in wr:
                dr = wr["DeleteRequest"]
                if dr == None or type(dr) != "dict":
                    return _validation_err("Invalid DeleteRequest")
                kid, kerr = _key_id(tdoc, dr.get("Key", None))
                if kerr != "":
                    return _validation_err(kerr)
                plan.append(["del", kid])
            else:
                return _validation_err("One or more parameter values were invalid: each write request must be exactly one of PutRequest/DeleteRequest")
        cap = _capacity(body, tname)
        if cap != None:
            caps.append(cap)
    # Pass 2: apply the validated plan.
    ic = _items()
    for op in plan:
        if op[0] == "put":
            _upsert_item(op[1], op[2], op[3], op[4])
        elif ic.get(op[1]) != None:
            ic.delete(op[1])
    out = {"UnprocessedItems": {}}
    if len(caps) > 0:
        out["ConsumedCapacity"] = caps
    return respond(200, out)
