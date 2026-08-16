# User handlers — Braze REST API.
#
# POST /users/track      → ingest user data (attributes, events, purchases)
# POST /users/alias/new  → create user aliases (real new/existing semantics)
# POST /users/identify   → merge alias-only profiles into identified profiles
# POST /users/export/ids → export profiles by identifier
# POST /users/delete     → hard-delete profiles by identifier
#
# Shared helpers (_require_auth, _body_of, _bad_body, _fatal, _upsert_user,
# _resolve_user, _apply_attributes, _export_user, _parse_iso8601, ...) are
# preloaded from scripts/lib.star.

_MAX_TRACK_IDS = 75     # /users/track: max external ids per request
_MAX_REQUEST_IDS = 50   # alias/new, identify, export/ids, delete: max ids per request
_MAX_EXT_ID_BYTES = 987 # EXTERNAL_USER_ID_TOO_LARGE

def _track_array(body, key):
    v = body.get(key, [])
    if v == None:
        return []
    if type(v) != "list":
        return []
    return v

# _valid_email backs the documented EMAIL_BAD_FORMAT check: the value must
# look like an address (local part, @, dotted domain).
def _valid_email(e):
    if e == None or type(e) != "string":
        return False
    at = -1
    for i in range(len(e)):
        if e[i] == "@":
            if at >= 0:
                return False
            at = i
        elif e[i] == " ":
            return False
    if at <= 0 or at >= len(e) - 3:
        return False
    dot = -1
    for i in range(at + 1, len(e)):
        if e[i] == ".":
            dot = i
    return dot > at + 1 and dot < len(e) - 1

_SUB_STATES = ["subscribed", "unsubscribed", "opted_in"]

# _reserved_prop reports the first reserved key used in a properties object
# (real error "Invalid 'properties' field"), or None.
def _reserved_prop(props, reserved):
    if props == None:
        return None
    for k in reserved:
        if k in props:
            return k
    return None

_EVENT_RESERVED_PROPS = ["time", "event_name"]
_PURCHASE_RESERVED_PROPS = ["time", "product_id", "quantity", "event_name", "price", "currency"]

def on_track(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body, ok = _body_of(req)
    if not ok:
        return _bad_body()

    attributes = _track_array(body, "attributes")
    events = _track_array(body, "events")
    purchases = _track_array(body, "purchases")

    # Fatal error "Max Input Length Exceeded": more than 75 external ids in
    # one /users/track request.
    seen = {}
    ids = 0
    for rec in attributes + events + purchases:
        if rec == None or type(rec) != "dict":
            continue
        eid = rec.get("external_id", None)
        alias = rec.get("user_alias", None)
        key = ""
        if eid != None and eid != "":
            key = "e:" + str(eid)
        elif alias != None and type(alias) == "dict":
            key = "a:" + _alias_key(alias)
        if key != "" and key not in seen:
            seen[key] = 1
            ids = ids + 1
    if ids > _MAX_TRACK_IDS:
        return _fatal("Max Input Length Exceeded",
            "Caused by calling more than " + str(_MAX_TRACK_IDS) +
            " external ids when hitting the User Track endpoint.")

    errors = []
    attributes_processed = 0
    events_processed = 0
    purchases_processed = 0

    uc = store_collection("users")
    ec = store_collection("events")
    pc = store_collection("purchases")

    # --- attributes ---
    for i in range(len(attributes)):
        rec = attributes[i]
        if rec == None or type(rec) != "dict":
            errors.append({"BAD_REQUEST": "attributes[" + str(i) + "]: Bad syntax. Each attribute must be an object."})
            continue
        eid = rec.get("external_id", None)
        if eid != None and len(str(eid)) > _MAX_EXT_ID_BYTES:
            errors.append({"EXTERNAL_USER_ID_TOO_LARGE": "attributes[" + str(i) + "]: external_id exceeds " + str(_MAX_EXT_ID_BYTES) + " bytes"})
            continue
        email = rec.get("email", None)
        if email != None and not _valid_email(email):
            errors.append({"EMAIL_BAD_FORMAT": "attributes[" + str(i) + "]: Invalid email address " + str(email)})
            continue
        sub = rec.get("email_subscribe", None)
        if sub != None and sub not in _SUB_STATES:
            errors.append({"BAD_EMAIL_SUBSCRIPTION_STATE": "attributes[" + str(i) + "]: email_subscribe must be subscribed, unsubscribed, or opted_in"})
            continue
        psub = rec.get("push_subscribe", None)
        if psub != None and psub not in _SUB_STATES:
            errors.append({"BAD_PUSH_SUBSCRIPTION_STATE": "attributes[" + str(i) + "]: push_subscribe must be subscribed, unsubscribed, or opted_in"})
            continue
        profile = _track_target(uc, rec)
        if profile == None:
            if rec.get("_update_existing_only", False):
                continue
            errors.append({"BAD_REQUEST": "attributes[" + str(i) + "]: you must include one of external_id, user_alias, braze_id, email, or phone"})
            continue
        _apply_attributes(profile, rec)
        uc.update(profile["id"], profile)
        attributes_processed = attributes_processed + 1

    # --- events ---
    for i in range(len(events)):
        rec = events[i]
        if rec == None or type(rec) != "dict":
            errors.append({"BAD_REQUEST": "events[" + str(i) + "]: Bad syntax. Each event must be an object."})
            continue
        name = rec.get("name", None)
        if name == None or name == "":
            errors.append({"BAD_REQUEST": "events[" + str(i) + "]: you must include a name"})
            continue
        time_s = rec.get("time", None)
        t = _parse_iso8601(time_s)
        if t == None:
            errors.append({"BAD_REQUEST": "events[" + str(i) + "]: 'time' must be a datetime as string in ISO 8601"})
            continue
        props = rec.get("properties", None)
        if props != None and type(props) != "dict":
            errors.append({"BAD_REQUEST": "events[" + str(i) + "]: Invalid 'properties' field. properties must be an object."})
            continue
        bad = _reserved_prop(props, _EVENT_RESERVED_PROPS)
        if bad != None:
            errors.append({"Invalid 'properties' field": "events[" + str(i) + "]: reserved key '" + bad + "' cannot be used as a custom event property name"})
            continue
        profile = _track_target(uc, rec)
        if profile == None:
            if rec.get("_update_existing_only", False):
                continue
            errors.append({"BAD_REQUEST": "events[" + str(i) + "]: you must include one of external_id, user_alias, braze_id, email, or phone"})
            continue
        ec.insert({
            "_user": profile["id"],
            "external_id": profile.get("external_id", None),
            "app_id": rec.get("app_id", None),
            "name": name,
            "time": time_s,
            "_t": t,
            "properties": props,
        })
        events_processed = events_processed + 1

    # --- purchases ---
    for i in range(len(purchases)):
        rec = purchases[i]
        if rec == None or type(rec) != "dict":
            errors.append({"BAD_REQUEST": "purchases[" + str(i) + "]: Bad syntax. Each purchase must be an object."})
            continue
        product_id = rec.get("product_id", None)
        if product_id == None or product_id == "":
            errors.append({"BAD_REQUEST": "purchases[" + str(i) + "]: you must include a product_id"})
            continue
        currency = rec.get("currency", None)
        if currency == None or type(currency) != "string" or len(currency) != 3 or currency != currency.upper():
            errors.append({"BAD_REQUEST": "purchases[" + str(i) + "]: currency must be an ISO 4217 Alphabetic Currency Code"})
            continue
        price = rec.get("price", None)
        if price == None or (type(price) != "int" and type(price) != "float"):
            errors.append({"BAD_REQUEST": "purchases[" + str(i) + "]: you must include a price"})
            continue
        time_s = rec.get("time", None)
        t = _parse_iso8601(time_s)
        if t == None:
            errors.append({"BAD_REQUEST": "purchases[" + str(i) + "]: 'time' must be a datetime as string in ISO 8601"})
            continue
        quantity = rec.get("quantity", 1)
        if quantity == None:
            quantity = 1
        if type(quantity) == "float":
            quantity = int(quantity)
        if type(quantity) != "int" or quantity < 1 or quantity > 100:
            errors.append({"BAD_REQUEST": "purchases[" + str(i) + "]: quantity must be between 1 and 100"})
            continue
        props = rec.get("properties", None)
        if props != None and type(props) != "dict":
            errors.append({"BAD_REQUEST": "purchases[" + str(i) + "]: Invalid 'properties' field. properties must be an object."})
            continue
        bad = _reserved_prop(props, _PURCHASE_RESERVED_PROPS)
        if bad != None:
            errors.append({"Invalid 'properties' field": "purchases[" + str(i) + "]: reserved key '" + bad + "' cannot be used as a purchase property name"})
            continue
        profile = _track_target(uc, rec)
        if profile == None:
            if rec.get("_update_existing_only", False):
                continue
            errors.append({"BAD_REQUEST": "purchases[" + str(i) + "]: you must include one of external_id, user_alias, braze_id, email, or phone"})
            continue
        pc.insert({
            "_user": profile["id"],
            "external_id": profile.get("external_id", None),
            "app_id": rec.get("app_id", None),
            "product_id": product_id,
            "currency": currency,
            "price": price,
            "quantity": quantity,
            "time": time_s,
            "_t": t,
            "properties": props,
        })
        purchases_processed = purchases_processed + 1

    # Per-record status: records that did not ingest are reported in the
    # real non-fatal errors array; message stays "success" and every
    # unaffected record was delivered.
    out = {
        "message": "success",
        "attributes_processed": attributes_processed,
        "events_processed": events_processed,
        "purchases_processed": purchases_processed,
    }
    if len(errors) > 0:
        out["errors"] = errors
    return respond(200, out)

def on_alias_new(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body, ok = _body_of(req)
    if not ok:
        return _bad_body()

    aliases = _track_array(body, "user_aliases")
    if len(aliases) > _MAX_REQUEST_IDS:
        return _fatal("The max number of ids per request was exceeded",
            "Up to " + str(_MAX_REQUEST_IDS) + " user aliases per request.")

    errors = []
    uc = store_collection("users")

    for i in range(len(aliases)):
        a = aliases[i]
        if a == None or type(a) != "dict":
            errors.append({"BAD_REQUEST": "user_aliases[" + str(i) + "]: Bad syntax. Each alias must be an object."})
            continue
        name = a.get("alias_name", None)
        label = a.get("alias_label", None)
        if name == None or label == None or name == "" or label == "" or type(name) != "string" or type(label) != "string":
            errors.append({"BAD_REQUEST": "user_aliases[" + str(i) + "]: you must include both an alias_name and an alias_label"})
            continue
        alias = {"alias_name": name, "alias_label": label}
        eid = a.get("external_id", None)
        if eid != None and eid != "":
            # Adding an alias for an existing user requires an external_id;
            # when no user has it the real API adds the alias to no user.
            profile = uc.get(str(eid))
            if profile == None:
                continue
            existing = _find_by_alias(uc, alias)
            if existing != None and existing.get("id", "") != profile.get("id", ""):
                errors.append({"BAD_REQUEST": "user_aliases[" + str(i) + "]: the alias_label and alias_name pair must be unique across your user base"})
                continue
            if existing != None:
                continue  # same user already carries it: success, no change
            current = profile.get("user_aliases", [])
            if current == None:
                current = []
            current.append(alias)
            profile["user_aliases"] = current
            uc.update(profile["id"], profile)
        else:
            # No external_id: create a new alias-only user.
            existing = _find_by_alias(uc, alias)
            if existing != None:
                continue  # alias already exists: success, no change
            doc = _new_profile(None, alias)
            uc.insert(doc)

    out = {
        "aliases_processed": len(aliases),
        "message": "success",
    }
    if len(errors) > 0:
        out["errors"] = errors
    return respond(200, out)

def on_identify(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body, ok = _body_of(req)
    if not ok:
        return _bad_body()

    to_identify = _track_array(body, "aliases_to_identify")
    if len(to_identify) > _MAX_REQUEST_IDS:
        return _fatal("The max number of ids per request was exceeded",
            "Up to " + str(_MAX_REQUEST_IDS) + " aliases_to_identify per request.")

    errors = []
    uc = store_collection("users")
    ec = store_collection("events")
    pc = store_collection("purchases")

    for i in range(len(to_identify)):
        entry = to_identify[i]
        if entry == None or type(entry) != "dict":
            errors.append({"BAD_REQUEST": "aliases_to_identify[" + str(i) + "]: Bad syntax. Each entry must be an object."})
            continue
        eid = entry.get("external_id", None)
        alias = entry.get("user_alias", None)
        if eid == None or eid == "" or alias == None or type(alias) != "dict":
            errors.append({"BAD_REQUEST": "aliases_to_identify[" + str(i) + "]: you must include both an external_id and a user_alias"})
            continue
        eid = str(eid)
        anon = _find_by_alias(uc, alias)
        if anon == None:
            # Nothing to identify (no alias-only profile carries the alias).
            continue
        target = uc.get(eid)
        if target == None:
            # No user with that external_id: the external_id is added to the
            # aliased user's record (the user becomes identified). The
            # profile is re-keyed, so its tracked events/purchases are
            # re-pointed at the new key.
            old_id = anon.get("id", "")
            anon["external_id"] = eid
            updated = {}
            for k in anon:
                updated[k] = anon[k]
            updated["id"] = eid
            uc.delete(old_id)
            uc.insert(updated)
            for doc in ec.list():
                if doc.get("_user", "") == old_id:
                    doc["_user"] = eid
                    doc["external_id"] = eid
                    ec.update(doc["id"], doc)
            for doc in pc.list():
                if doc.get("_user", "") == old_id:
                    doc["_user"] = eid
                    doc["external_id"] = eid
                    pc.update(doc["id"], doc)
            continue
        if target.get("id", "") == anon.get("id", ""):
            continue
        # Both exist: merge the alias-only profile into the identified one
        # (attributes, events, purchases, aliases), then remove the
        # alias-only profile.
        aliases = target.get("user_aliases", [])
        if aliases == None:
            aliases = []
        for al in anon.get("user_aliases", []):
            dup = False
            for cur in aliases:
                if cur.get("alias_label", "") == al.get("alias_label", "") and cur.get("alias_name", "") == al.get("alias_name", ""):
                    dup = True
                    break
            if not dup:
                aliases.append(al)
        target["user_aliases"] = aliases
        ca = target.get("custom_attributes", {})
        if ca == None:
            ca = {}
        anon_ca = anon.get("custom_attributes", {})
        if anon_ca != None:
            for k in anon_ca:
                if k not in ca:
                    ca[k] = anon_ca[k]
        target["custom_attributes"] = ca
        for fld in _PROFILE_FIELDS:
            if target.get(fld, None) == None and anon.get(fld, None) != None:
                target[fld] = anon[fld]
        uc.update(target["id"], target)
        for doc in ec.list():
            if doc.get("_user", "") == anon.get("id", ""):
                doc["_user"] = target["id"]
                doc["external_id"] = target.get("external_id", None)
                ec.update(doc["id"], doc)
        for doc in pc.list():
            if doc.get("_user", "") == anon.get("id", ""):
                doc["_user"] = target["id"]
                doc["external_id"] = target.get("external_id", None)
                pc.update(doc["id"], doc)
        uc.delete(anon["id"])

    out = {
        "aliases_processed": len(to_identify),
        "message": "success",
    }
    if len(errors) > 0:
        out["errors"] = errors
    return respond(200, out)

def on_export_ids(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body, ok = _body_of(req)
    if not ok:
        return _bad_body()

    external_ids = _track_array(body, "external_ids")
    user_aliases = _track_array(body, "user_aliases")
    fields = body.get("fields_to_export", None)
    if fields == None or type(fields) != "list":
        fields = None

    if len(external_ids) == 0 and len(user_aliases) == 0:
        return _fatal("Bad Request",
            "You must include one of external_ids, user_aliases, braze_ids, email_address, or phone.")
    if len(external_ids) + len(user_aliases) > _MAX_REQUEST_IDS:
        return _fatal("The max number of ids per request was exceeded",
            "You can only request up to " + str(_MAX_REQUEST_IDS) + " external_ids or user_aliases.")

    uc = store_collection("users")
    users = []
    invalid = []
    seen = {}
    for eid in external_ids:
        eid = str(eid)
        if eid in seen:
            continue
        seen[eid] = 1
        profile = uc.get(eid)
        if profile == None:
            invalid.append(eid)
            continue
        users.append(_export_user(profile, fields))
    for alias in user_aliases:
        if alias == None or type(alias) != "dict":
            continue
        profile = _find_by_alias(uc, alias)
        if profile == None:
            invalid.append(alias.get("alias_name", ""))
            continue
        users.append(_export_user(profile, fields))

    return respond(200, {
        "message": "success",
        "users": users,
        "invalid_user_ids": invalid,
    })

def on_delete(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    body, ok = _body_of(req)
    if not ok:
        return _bad_body()

    external_ids = _track_array(body, "external_ids")
    user_aliases = _track_array(body, "user_aliases")
    if len(external_ids) == 0 and len(user_aliases) == 0:
        return _fatal("Bad Request",
            "You must include one of external_ids, user_aliases, braze_ids, email_addresses, or phone_numbers.")
    if len(external_ids) > 0 and len(user_aliases) > 0:
        return _fatal("Bad Request",
            "Only one identifier type can be used per request.")
    if len(external_ids) + len(user_aliases) > _MAX_REQUEST_IDS:
        return _fatal("The max number of ids per request was exceeded",
            "You can only request up to " + str(_MAX_REQUEST_IDS) + " identifiers per request.")

    uc = store_collection("users")
    ec = store_collection("events")
    pc = store_collection("purchases")

    targets = []
    if len(external_ids) > 0:
        for eid in external_ids:
            profile = uc.get(str(eid))
            if profile != None:
                targets.append(profile)
    else:
        for alias in user_aliases:
            if alias == None or type(alias) != "dict":
                continue
            profile = _find_by_alias(uc, alias)
            if profile != None:
                targets.append(profile)

    deleted = 0
    for profile in targets:
        pid = profile.get("id", "")
        for doc in ec.list():
            if doc.get("_user", "") == pid:
                ec.delete(doc["id"])
        for doc in pc.list():
            if doc.get("_user", "") == pid:
                pc.delete(doc["id"])
        uc.delete(pid)
        deleted = deleted + 1

    return respond(200, {
        "deleted": deleted,
    })
