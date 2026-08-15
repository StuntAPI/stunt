# Firewall rule and page rule handlers for the Cloudflare API.
#
# Firewall rules (the real zone-level Ruleset "firewall/rules" surface):
#   GET    /zones/{zone_id}/firewall/rules              -> list rules
#   POST   /zones/{zone_id}/firewall/rules              -> create rule(s)
#   GET    /zones/{zone_id}/firewall/rules/{rule_id}    -> get rule
#   PUT    /zones/{zone_id}/firewall/rules/{rule_id}    -> replace rule
#   PATCH  /zones/{zone_id}/firewall/rules/{rule_id}    -> partial update
#   DELETE /zones/{zone_id}/firewall/rules/{rule_id}    -> delete rule
#
# Page rules:
#   GET    /zones/{zone_id}/page_rules                  -> list rules
#   POST   /zones/{zone_id}/page_rules                  -> create rule
#   GET    /zones/{zone_id}/page_rules/{rule_id}        -> get rule
#   PUT    /zones/{zone_id}/page_rules/{rule_id}        -> replace rule
#   PATCH  /zones/{zone_id}/page_rules/{rule_id}        -> partial update
#   DELETE /zones/{zone_id}/page_rules/{rule_id}        -> delete rule
#
# Stateful: rules are persisted in the "firewall_rules" / "page_rules"
# collections; the first access against a zone seeds one canned rule of each
# kind (what the previous constant responses returned).
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_id, _find_zone) are
# preloaded from scripts/lib.star.

# Cloudflare's "Firewall rule not found" error code, assembled (no 5+ digit
# literals in scripts).
_FW_ERR_NOT_FOUND = 10 * 1000 + 35

# Cloudflare's firewall rule actions.
_FW_ACTIONS = ["block", "challenge", "js_challenge", "managed_challenge", "allow", "log", "skip"]

# Page rule statuses.
_PR_STATUSES = ["active", "paused", "deleted"]

# ====================================================================
# Firewall rules
# ====================================================================

# on_list_firewall_rules returns the firewall rules for a zone.
def on_list_firewall_rules(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone = _find_zone(req["params"]["zone_id"])
    if zone == None:
        return _cf_err(404, 1003, "Zone not found.")

    _ensure_seed_firewall(zone)
    frc = store_collection("firewall_rules")
    result = []
    for r in frc.list():
        if r.get("zone_id", "") == req["params"]["zone_id"]:
            result.append(_fw_result(r))

    page, next_cursor = _list_page(req, result)
    return _cf_ok_with_info(page, len(result), next_cursor)

# on_create_firewall_rules creates one firewall rule (single object) or a
# batch (the real API accepts an array of rules in the body).
def on_create_firewall_rules(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone = _find_zone(req["params"]["zone_id"])
    if zone == None:
        return _cf_err(404, 1003, "Zone not found.")

    body = req.get("body")
    if body == None:
        return _cf_err(400, 1004, "Validation error: request body is required.")

    rules = body
    if type(body) == "dict":
        # The engine hands JSON array bodies through wrapped under the
        # reserved "_batch" key.
        batch = body.get("_batch", None)
        if batch != None and type(batch) == "list":
            rules = batch
        else:
            rules = [body]
    if type(rules) != "list" or len(rules) == 0:
        return _cf_err(400, 1004, "Validation error: at least one rule is required.")

    frc = store_collection("firewall_rules")
    created = []
    for b in rules:
        if type(b) != "dict":
            return _cf_err(400, 1004, "Validation error: each rule must be an object.")
        doc, verr = _fw_validated(b, None, False)
        if verr != None:
            return verr
        doc["rule_id"] = _gen_id("fw")
        doc["zone_id"] = req["params"]["zone_id"]
        doc["filter_id"] = _gen_id("filter")
        doc["created_on"] = _iso8601()
        doc["modified_on"] = _iso8601()
        frc.insert(doc)
        created.append(_fw_result(doc))

    return _cf_ok(created)

# on_get_firewall_rule returns a single firewall rule.
def on_get_firewall_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    rule = _find_rule("firewall_rules", "rule_id", req["params"]["zone_id"], req["params"]["rule_id"])
    if rule == None:
        return _cf_err(404, _FW_ERR_NOT_FOUND, "Firewall rule not found.")
    return _cf_ok(_fw_result(rule))

# on_update_firewall_rule replaces a firewall rule.
def on_update_firewall_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_rule("firewall_rules", "rule_id", req["params"]["zone_id"], req["params"]["rule_id"])
    if existing == None:
        return _cf_err(404, _FW_ERR_NOT_FOUND, "Firewall rule not found.")

    body = req.get("body")
    if body == None:
        return _cf_err(400, 1004, "Validation error: request body is required.")

    doc, verr = _fw_validated(body, existing, True)
    if verr != None:
        return verr

    merged = _fw_merge_doc(existing, doc)
    frc = store_collection("firewall_rules")
    frc.update(existing.get("id", ""), merged)
    return _cf_ok(_fw_result(merged))

# on_patch_firewall_rule partially updates a firewall rule.
def on_patch_firewall_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_rule("firewall_rules", "rule_id", req["params"]["zone_id"], req["params"]["rule_id"])
    if existing == None:
        return _cf_err(404, _FW_ERR_NOT_FOUND, "Firewall rule not found.")

    body = req.get("body")
    if body == None:
        return _cf_err(400, 1004, "Validation error: request body is required.")

    doc, verr = _fw_validated(body, existing, False)
    if verr != None:
        return verr

    merged = _fw_merge_doc(existing, doc)
    frc = store_collection("firewall_rules")
    frc.update(existing.get("id", ""), merged)
    return _cf_ok(_fw_result(merged))

# on_delete_firewall_rule deletes a firewall rule.
# Real Cloudflare returns 200 with the deleted rule's id in the result.
def on_delete_firewall_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_rule("firewall_rules", "rule_id", req["params"]["zone_id"], req["params"]["rule_id"])
    if existing == None:
        return _cf_err(404, _FW_ERR_NOT_FOUND, "Firewall rule not found.")

    frc = store_collection("firewall_rules")
    frc.delete(existing.get("id", ""))
    return _cf_ok({"id": existing.get("rule_id", "")})

# _fw_validated validates firewall rule fields (falling back to the existing
# rule for absent fields — except under full=True, where the real PUT
# semantics require action and filter.expression in the body) and returns
# (fields, error_response).
def _fw_validated(body, existing, full):
    action = body.get("action", None)
    if action == None and not full and existing != None:
        action = existing.get("action", None)
    if action == None or action == "":
        return None, _cf_err(400, 1004, "Validation error: 'action' is required.")
    action = str(action)
    if action not in _FW_ACTIONS:
        return None, _cf_err(400, 1004, "Validation error: unknown action '" + action + "'.")

    expr = None
    filt = body.get("filter", None)
    if filt != None and type(filt) == "dict":
        expr = filt.get("expression", None)
    if expr == None:
        if full:
            return None, _cf_err(400, 1004, "Validation error: 'filter.expression' is required.")
        if existing != None:
            expr = existing.get("expression", None)
    if expr == None or expr == "":
        return None, _cf_err(400, 1004, "Validation error: 'filter.expression' is required.")
    expr = str(expr)

    description = body.get("description", None)
    if description == None and existing != None:
        description = existing.get("description", None)
    if description == None:
        description = ""

    paused = body.get("paused", None)
    if paused == None and existing != None:
        paused = existing.get("paused", None)
    if paused == None:
        paused = False

    priority = body.get("priority", None)
    if priority == None and existing != None:
        priority = existing.get("priority", None)

    doc = {
        "action": action,
        "expression": expr,
        "description": description,
        "paused": paused,
    }
    if priority != None:
        doc["priority"] = priority
    return doc, None

# _fw_merge_doc merges validated fields over the existing stored rule,
# preserving identity fields and stamping modified_on.
def _fw_merge_doc(existing, doc):
    merged = {
        "rule_id": existing.get("rule_id", ""),
        "zone_id": existing.get("zone_id", ""),
        "filter_id": existing.get("filter_id", ""),
        "created_on": existing.get("created_on", _iso8601()),
        "modified_on": _iso8601(),
    }
    for key in ["action", "expression", "description", "paused", "priority"]:
        if doc.get(key, None) != None:
            merged[key] = doc.get(key)
    return merged

# _fw_result returns a clean firewall rule object (the real envelope nests
# the expression under filter).
def _fw_result(r):
    res = {
        "id": r.get("rule_id", ""),
        "paused": r.get("paused", False),
        "description": r.get("description", ""),
        "action": r.get("action", ""),
        "filter": {
            "id": r.get("filter_id", ""),
            "expression": r.get("expression", ""),
            "paused": False,
        },
        "created_on": r.get("created_on", _iso8601()),
        "modified_on": r.get("modified_on", _iso8601()),
    }
    if r.get("priority", None) != None:
        res["priority"] = r.get("priority")
    return res

# _ensure_seed_firewall seeds the canned firewall rule for a zone once.
def _ensure_seed_firewall(zone):
    zone_id = zone.get("zone_id", "")
    if store_kv_get("cf", "fw_seed_" + zone_id) == "1":
        return
    frc = store_collection("firewall_rules")
    for r in frc.list():
        if r.get("zone_id", "") == zone_id:
            store_kv_set("cf", "fw_seed_" + zone_id, "1")
            return
    frc.insert({
        "rule_id": _gen_id("fw"),
        "zone_id": zone_id,
        "description": "Block known bad IPs",
        "action": "block",
        "expression": "(ip.src eq 192.0.2.0/24)",
        "paused": False,
        "filter_id": _gen_id("filter"),
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    })
    store_kv_set("cf", "fw_seed_" + zone_id, "1")

# ====================================================================
# Page rules
# ====================================================================

# on_list_page_rules returns the page rules for a zone.
def on_list_page_rules(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone = _find_zone(req["params"]["zone_id"])
    if zone == None:
        return _cf_err(404, 1003, "Zone not found.")

    _ensure_seed_page_rules(zone)
    prc = store_collection("page_rules")
    result = []
    for r in prc.list():
        if r.get("zone_id", "") == req["params"]["zone_id"]:
            result.append(_pr_result(r))

    # Real List Page Rules filter (status), applied before paging.
    result = _apply_page_rule_filters(req, result)

    page, next_cursor = _list_page(req, result)
    return _cf_ok_with_info(page, len(result), next_cursor)

# on_create_page_rule creates a page rule.
def on_create_page_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone = _find_zone(req["params"]["zone_id"])
    if zone == None:
        return _cf_err(404, 1003, "Zone not found.")

    body = req.get("body")
    if body == None or type(body) != "dict":
        return _cf_err(400, 1004, "Validation error: request body is required.")

    doc, verr = _pr_validated(body, None, False)
    if verr != None:
        return verr

    doc["rule_id"] = _gen_id("pr")
    doc["zone_id"] = req["params"]["zone_id"]
    doc["priority"] = _pr_next_priority(req["params"]["zone_id"])
    doc["created_on"] = _iso8601()
    doc["modified_on"] = _iso8601()

    prc = store_collection("page_rules")
    prc.insert(doc)
    return _cf_ok(_pr_result(doc))

# on_get_page_rule returns a single page rule.
def on_get_page_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    rule = _find_rule("page_rules", "rule_id", req["params"]["zone_id"], req["params"]["rule_id"])
    if rule == None:
        return _cf_err(404, 1002, "Page rule not found.")
    return _cf_ok(_pr_result(rule))

# on_update_page_rule replaces a page rule (targets/actions required).
def on_update_page_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_rule("page_rules", "rule_id", req["params"]["zone_id"], req["params"]["rule_id"])
    if existing == None:
        return _cf_err(404, 1002, "Page rule not found.")

    body = req.get("body")
    if body == None or type(body) != "dict":
        return _cf_err(400, 1004, "Validation error: request body is required.")

    doc, verr = _pr_validated(body, existing, True)
    if verr != None:
        return verr

    merged = _pr_merge_doc(existing, doc)
    prc = store_collection("page_rules")
    prc.update(existing.get("id", ""), merged)
    return _cf_ok(_pr_result(merged))

# on_patch_page_rule partially updates a page rule.
def on_patch_page_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_rule("page_rules", "rule_id", req["params"]["zone_id"], req["params"]["rule_id"])
    if existing == None:
        return _cf_err(404, 1002, "Page rule not found.")

    body = req.get("body")
    if body == None or type(body) != "dict":
        return _cf_err(400, 1004, "Validation error: request body is required.")

    doc, verr = _pr_validated(body, existing, False)
    if verr != None:
        return verr

    merged = _pr_merge_doc(existing, doc)
    prc = store_collection("page_rules")
    prc.update(existing.get("id", ""), merged)
    return _cf_ok(_pr_result(merged))

# on_delete_page_rule deletes a page rule.
# Real Cloudflare returns 200 with the deleted rule's id in the result.
def on_delete_page_rule(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_rule("page_rules", "rule_id", req["params"]["zone_id"], req["params"]["rule_id"])
    if existing == None:
        return _cf_err(404, 1002, "Page rule not found.")

    prc = store_collection("page_rules")
    prc.delete(existing.get("id", ""))
    return _cf_ok({"id": existing.get("rule_id", "")})

# _pr_validated validates page rule fields (falling back to the existing rule
# for absent fields) and returns (fields, error_response).
def _pr_validated(body, existing, full):
    targets = body.get("targets", None)
    if targets == None and existing != None:
        targets = existing.get("targets", None)
    if targets == None or type(targets) != "list" or len(targets) == 0:
        return None, _cf_err(400, 1004, "Validation error: 'targets' must be a non-empty array.")

    actions = body.get("actions", None)
    if actions == None and existing != None:
        actions = existing.get("actions", None)
    if actions == None or type(actions) != "list" or len(actions) == 0:
        return None, _cf_err(400, 1004, "Validation error: 'actions' must be a non-empty array.")

    status = body.get("status", None)
    if status == None and existing != None:
        status = existing.get("status", None)
    if status == None or status == "":
        status = "active"
    status = str(status)
    if status not in _PR_STATUSES:
        return None, _cf_err(400, 1004, "Validation error: 'status' must be one of active, paused, deleted.")

    return {"targets": targets, "actions": actions, "status": status}, None

# _pr_merge_doc merges validated fields over the existing stored rule,
# preserving identity fields and stamping modified_on.
def _pr_merge_doc(existing, doc):
    return {
        "rule_id": existing.get("rule_id", ""),
        "zone_id": existing.get("zone_id", ""),
        "priority": existing.get("priority", 1),
        "created_on": existing.get("created_on", _iso8601()),
        "modified_on": _iso8601(),
        "targets": doc.get("targets", []),
        "actions": doc.get("actions", []),
        "status": doc.get("status", "active"),
    }

# _pr_result returns a clean page rule object for the API response.
def _pr_result(r):
    return {
        "id": r.get("rule_id", ""),
        "targets": r.get("targets", []),
        "actions": r.get("actions", []),
        "priority": r.get("priority", 1),
        "status": r.get("status", "active"),
        "created_on": r.get("created_on", _iso8601()),
        "modified_on": r.get("modified_on", _iso8601()),
    }

# _pr_next_priority returns max(existing priority) + 1 for a zone (the real
# API assigns priorities this way when the field is omitted).
def _pr_next_priority(zone_id):
    prc = store_collection("page_rules")
    max_p = 0
    for r in prc.list():
        if r.get("zone_id", "") == zone_id:
            p = r.get("priority", 0)
            if p > max_p:
                max_p = p
    return max_p + 1

# _ensure_seed_page_rules seeds the canned page rule for a zone once.
def _ensure_seed_page_rules(zone):
    zone_id = zone.get("zone_id", "")
    if store_kv_get("cf", "pr_seed_" + zone_id) == "1":
        return
    prc = store_collection("page_rules")
    for r in prc.list():
        if r.get("zone_id", "") == zone_id:
            store_kv_set("cf", "pr_seed_" + zone_id, "1")
            return
    zone_name = zone.get("name", "stunt.dev")
    prc.insert({
        "rule_id": _gen_id("pr"),
        "zone_id": zone_id,
        "targets": [{"target": "url", "constraint": {"operator": "matches", "value": zone_name + "/*"}}],
        "actions": [{"id": "browser_cache_ttl", "value": 300}],
        "priority": 1,
        "status": "active",
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    })
    store_kv_set("cf", "pr_seed_" + zone_id, "1")

# _apply_page_rule_filters maps the real Cloudflare List Page Rules query
# param (status: active/paused/deleted) to a query_select clause, applied
# before paging like the real API.
def _apply_page_rule_filters(req, rules):
    status = _get_query(req, "status", "")
    if status == "":
        return rules
    return query_select(rules, [["status", "=", status]])

# _find_rule returns the stored rule doc from the given collection matching
# zone_id + rule_id, or None.
def _find_rule(coll, id_field, zone_id, rule_id):
    rc = store_collection(coll)
    for r in rc.list():
        if r.get("zone_id", "") == zone_id and r.get(id_field, "") == rule_id:
            return r
    return None
