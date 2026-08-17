# DNS record handlers for the Cloudflare API.
#
# GET    /zones/{zone_id}/dns_records                    -> list records
# POST   /zones/{zone_id}/dns_records                    -> create record
# GET    /zones/{zone_id}/dns_records/{dns_record_id}    -> get record
# PUT    /zones/{zone_id}/dns_records/{dns_record_id}    -> replace record
# PATCH  /zones/{zone_id}/dns_records/{dns_record_id}    -> partial update
# DELETE /zones/{zone_id}/dns_records/{dns_record_id}    -> delete record
#
# Stateful: records are persisted in the "dns_records" collection; the first
# list against a zone seeds the three canned records. Validation mirrors the
# real API: type must be in Cloudflare's supported set, ttl must be 1 (auto)
# or between 60 and one day (60 * 60 * 24), and proxied records are forced
# to ttl 1.
#
# Shared helpers (_require_auth, _cf_ok, _cf_err, _gen_id, _find_zone) are
# preloaded from scripts/lib.star.

# Cloudflare's "Record does not exist" error code, assembled (no 5+ digit
# literals in scripts).
_DNS_ERR_NOT_FOUND = 81 * 1000 + 44

# Cloudflare's supported DNS record types.
_DNS_TYPES = [
    "A", "AAAA", "CAA", "CNAME", "DNSKEY", "DS", "HTTPS", "LOC",
    "MX", "NAPTR", "NS", "PTR", "SMIMEA", "SRV", "SSHFP", "SVCB",
    "TLSA", "TXT", "URI",
]

# on_list_dns_records returns the DNS records for a zone.
def on_list_dns_records(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone_id = req["params"]["zone_id"]
    zone = _find_zone(zone_id)
    if zone == None:
        return _cf_err(404, 1003, "Zone not found.")

    _ensure_seed_dns(zone)
    rc = store_collection("dns_records")
    records = []
    for r in rc.list():
        if r.get("zone_id", "") == zone_id:
            records.append(_dns_result(r))

    # Real List DNS Records filters (type/name/content), applied before
    # paging like the real API.
    records = _apply_dns_record_filters(req, records)

    page, next_cursor = _list_page(req, records)
    if page == None:
        return _cf_err(400, 400, "Invalid cursor token")
    return _cf_ok_with_info(page, len(records), next_cursor)

# on_create_dns_record creates a DNS record.
# POST /zones/{zone_id}/dns_records
def on_create_dns_record(req):
    err = _require_auth(req)
    if err != None:
        return err

    zone_id = req["params"]["zone_id"]
    zone = _find_zone(zone_id)
    if zone == None:
        return _cf_err(404, 1003, "Zone not found.")

    rec, verr = _dns_validated(req.get("body"), None, True)
    if verr != None:
        return verr

    rec["record_id"] = _gen_id("dns")
    rec["zone_id"] = zone_id
    rec["zone_name"] = zone.get("name", "")
    rec["created_on"] = _iso8601()
    rec["modified_on"] = _iso8601()

    rc = store_collection("dns_records")
    rc.insert(rec)
    return _cf_ok(_dns_result(rec))

# on_get_dns_record returns a single DNS record.
def on_get_dns_record(req):
    err = _require_auth(req)
    if err != None:
        return err

    rec = _find_dns_record(req["params"]["zone_id"], req["params"]["dns_record_id"])
    if rec == None:
        return _cf_err(404, _DNS_ERR_NOT_FOUND, "Record does not exist.")
    return _cf_ok(_dns_result(rec))

# on_update_dns_record replaces a DNS record (PUT requires the full field
# set, like the real API).
def on_update_dns_record(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_dns_record(req["params"]["zone_id"], req["params"]["dns_record_id"])
    if existing == None:
        return _cf_err(404, _DNS_ERR_NOT_FOUND, "Record does not exist.")

    rec, verr = _dns_validated(req.get("body"), existing, True)
    if verr != None:
        return verr

    doc = _dns_merge_doc(existing, rec)
    rc = store_collection("dns_records")
    rc.update(existing.get("id", ""), doc)
    return _cf_ok(_dns_result(doc))

# on_patch_dns_record partially updates a DNS record.
def on_patch_dns_record(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_dns_record(req["params"]["zone_id"], req["params"]["dns_record_id"])
    if existing == None:
        return _cf_err(404, _DNS_ERR_NOT_FOUND, "Record does not exist.")

    rec, verr = _dns_validated(req.get("body"), existing, False)
    if verr != None:
        return verr

    doc = _dns_merge_doc(existing, rec)
    rc = store_collection("dns_records")
    rc.update(existing.get("id", ""), doc)
    return _cf_ok(_dns_result(doc))

# on_delete_dns_record deletes a DNS record.
# Real Cloudflare returns 200 with the deleted record's id in the result.
def on_delete_dns_record(req):
    err = _require_auth(req)
    if err != None:
        return err

    existing = _find_dns_record(req["params"]["zone_id"], req["params"]["dns_record_id"])
    if existing == None:
        return _cf_err(404, _DNS_ERR_NOT_FOUND, "Record does not exist.")

    rc = store_collection("dns_records")
    rc.delete(existing.get("id", ""))
    return _cf_ok({"id": existing.get("record_id", "")})

# ====================================================================
# Helpers
# ====================================================================

# _find_dns_record returns the stored record doc for zone_id + record_id,
# or None.
def _find_dns_record(zone_id, record_id):
    rc = store_collection("dns_records")
    for r in rc.list():
        if r.get("zone_id", "") == zone_id and r.get("record_id", "") == record_id:
            return r
    return None

# _dns_cur resolves a field from the request body, falling back to the
# existing record (PATCH) and then to default_val.
def _dns_cur(body, existing, key, default_val):
    v = body.get(key, None)
    if v == None and existing != None:
        v = existing.get(key, None)
    if v == None:
        return default_val
    return v

# _dns_validated validates the request body (optionally merged over an
# existing record) and returns (record_fields, error_response). full=True
# requires type/name/content (the real PUT semantics); full=False keeps the
# existing values for absent fields (PATCH).
def _dns_validated(body, existing, full):
    if body == None:
        return None, _cf_err(400, 1004, "Validation error: request body is required.")

    # type
    rtype = body.get("type", None)
    if rtype == None and not full and existing != None:
        rtype = existing.get("type", None)
    if rtype == None or str(rtype) == "":
        if full:
            return None, _cf_err(400, 1004, "Validation error: 'type' is required.")
        rtype = "A"
    rtype = str(rtype).upper()
    if rtype not in _DNS_TYPES:
        return None, _cf_err(400, 1004, "Validation error: unknown record type '" + rtype + "'.")

    # name — required in the body under full=True (real PUT semantics);
    # falls back to the existing record for PATCH.
    name = body.get("name", None)
    if name == None and not full:
        name = _dns_cur(body, existing, "name", None)
    if name == None or str(name) == "":
        if full:
            return None, _cf_err(400, 1004, "Validation error: 'name' is required.")
        name = ""
    name = str(name)

    # content — same PUT/PATCH rules as name.
    content = body.get("content", None)
    if content == None and not full:
        content = _dns_cur(body, existing, "content", None)
    if content == None or str(content) == "":
        if full:
            return None, _cf_err(400, 1004, "Validation error: 'content' is required.")
        content = ""
    content = str(content)

    # proxied — real Cloudflare forces ttl to 1 (auto) on proxied records.
    proxied = _dns_cur(body, existing, "proxied", False)
    if proxied == None:
        proxied = False

    # ttl — 1 (auto) or between 60 and one day. JSON bodies carry whole
    # numbers as floats, so accept integral floats too.
    ttl = _dns_cur(body, existing, "ttl", 1)
    if ttl == None:
        ttl = 1
    if type(ttl) == "float":
        if ttl != int(ttl):
            return None, _cf_err(400, 1004, "Validation error: 'ttl' must be an integer.")
        ttl = int(ttl)
    if type(ttl) != "int":
        return None, _cf_err(400, 1004, "Validation error: 'ttl' must be an integer.")
    max_ttl = 60 * 60 * 24
    if ttl != 1 and (ttl < 60 or ttl > max_ttl):
        return None, _cf_err(400, 1004, "Validation error: 'ttl' must be 1 (auto) or between 60 and " + str(max_ttl) + ".")
    if proxied == True:
        ttl = 1

    # priority — required for MX/SRV/URI.
    priority = _dns_cur(body, existing, "priority", None)
    if rtype == "MX" or rtype == "SRV" or rtype == "URI":
        if priority == None:
            return None, _cf_err(400, 1004, "Validation error: 'priority' is required for " + rtype + " records.")

    rec = {
        "name": name,
        "type": rtype,
        "content": content,
        "proxied": proxied,
        "ttl": ttl,
    }
    if priority != None:
        rec["priority"] = priority
    return rec, None

# _dns_merge_doc merges validated fields over the existing stored record,
# preserving identity fields (record_id/zone_id/zone_name/created_on) and
# stamping modified_on.
def _dns_merge_doc(existing, rec):
    doc = {
        "record_id": existing.get("record_id", ""),
        "zone_id": existing.get("zone_id", ""),
        "zone_name": existing.get("zone_name", ""),
        "created_on": existing.get("created_on", _iso8601()),
        "modified_on": _iso8601(),
    }
    for key in ["name", "type", "content", "proxied", "ttl", "priority"]:
        if rec.get(key, None) != None:
            doc[key] = rec.get(key)
    return doc

# _dns_result returns a clean DNS record object for the API response (the
# real DNS Record envelope).
def _dns_result(r):
    res = {
        "id": r.get("record_id", ""),
        "zone_id": r.get("zone_id", ""),
        "zone_name": r.get("zone_name", ""),
        "name": r.get("name", ""),
        "type": r.get("type", ""),
        "content": r.get("content", ""),
        "proxied": r.get("proxied", False),
        "ttl": r.get("ttl", 1),
        "meta": {
            "auto_added": False,
            "managed_by_apps": False,
            "source": "primary",
        },
        "created_on": r.get("created_on", _iso8601()),
        "modified_on": r.get("modified_on", _iso8601()),
    }
    if r.get("priority", None) != None:
        res["priority"] = r.get("priority")
    return res

# _ensure_seed_dns seeds the canned records for a zone once (so lists still
# show the A/CNAME/MX baseline the way the canned version did).
def _ensure_seed_dns(zone):
    zone_id = zone.get("zone_id", "")
    if store_kv_get("cf", "dns_seed_" + zone_id) == "1":
        return
    rc = store_collection("dns_records")
    for r in rc.list():
        if r.get("zone_id", "") == zone_id:
            store_kv_set("cf", "dns_seed_" + zone_id, "1")
            return
    zone_name = zone.get("name", "example.com")
    rc.insert({
        "record_id": _gen_id("dns"),
        "zone_id": zone_id,
        "zone_name": zone_name,
        "name": zone_name,
        "type": "A",
        "content": "192.0.2.1",
        "proxied": True,
        "ttl": 1,
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    })
    rc.insert({
        "record_id": _gen_id("dns"),
        "zone_id": zone_id,
        "zone_name": zone_name,
        "name": "www." + zone_name,
        "type": "CNAME",
        "content": zone_name,
        "proxied": True,
        "ttl": 1,
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    })
    rc.insert({
        "record_id": _gen_id("dns"),
        "zone_id": zone_id,
        "zone_name": zone_name,
        "name": zone_name,
        "type": "MX",
        "content": "mail.stunt.dev",
        "priority": 10,
        "proxied": False,
        "ttl": 3600,
        "created_on": _iso8601(),
        "modified_on": _iso8601(),
    })
    store_kv_set("cf", "dns_seed_" + zone_id, "1")

# _apply_dns_record_filters maps the real Cloudflare List DNS Records query
# params (type/name/content exact matches, order/direction sorting) to
# query_select clauses, applied before paging like the real API.
def _apply_dns_record_filters(req, records):
    f = []
    rtype = _get_query(req, "type", "")
    if rtype != "":
        f.append(["type", "=", rtype])
    name = _get_query(req, "name", "")
    if name != "":
        f.append(["name", "=", name])
    content = _get_query(req, "content", "")
    if content != "":
        f.append(["content", "=", content])
    order = _get_query(req, "order", "")
    direction = _get_query(req, "direction", "asc")
    if len(f) == 0 and order == "":
        return records
    return query_select(records, f if len(f) > 0 else None, order, direction, None, None, None)
