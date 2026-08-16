# Shared library for braze-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). Helpers used here are defined here (load order).

# _check_auth validates Braze auth. Accepts either Bearer token or
# x-authorization header.
def _check_auth(req):
    # Bearer token
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    # x-authorization header
    xauth = req["headers"].get("x-authorization", "")
    if xauth != None and xauth != "":
        return xauth
    # Also check X-Authorization (Go canonicalizes)
    xauth2 = req["headers"].get("X-Authorization", "")
    if xauth2 != None and xauth2 != "":
        return xauth2
    return None

# Well-known static test API key, seeded once into the KV store on first
# request (see _seed_api_keys) so existing clients/tests that use it keep
# working while any other key is rejected with 401.
_TEST_API_KEY = "test-app-group-api-key"

# _seed_api_keys inserts the well-known test API key into the KV store
# exactly once per instance (guarded by the "auth_seeded" flag), stored
# under "tok:<key>" with a far-future expiry computed at runtime (Braze
# app-group API keys do not expire, so no hardcoded epoch is used).
def _seed_api_keys():
    if store_kv_get("braze", "auth_seeded") == "yes":
        return
    store_kv_set("braze", "auth_seeded", "yes")
    exp = str(clock.now_unix() + 3600 * 24 * 365 * 10)
    store_kv_set("braze", "tok:" + _TEST_API_KEY, exp)

# _require_auth returns (token, None) if the presented credential is a
# known, unexpired API key, or (None, error_response) if missing/unknown/
# expired.
def _require_auth(req):
    token = _check_auth(req)
    if token == None:
        return None, respond(401, {
            "message": "Unauthorized. A valid API key is required.",
        })
    _seed_api_keys()
    exp = store_kv_get("braze", "tok:" + token)
    if exp != None and clock.now_unix() <= _to_int(exp):
        return token, None
    return None, respond(401, {
        "message": "Unauthorized. A valid API key is required.",
    })

# _seed populates default segments and campaigns.
_SEGMENTS = [
    {"id": "seg001", "name": "Active Users", "status": "Active"},
    {"id": "seg002", "name": "Lapsed Users", "status": "Active"},
    {"id": "seg003", "name": "New Subscribers", "status": "Draft"},
]

# Seeded API-triggered campaigns (validated by /messages/send and
# /campaigns/trigger/send). Real Braze campaign and message-variation ids
# are UUIDs minted by the dashboard; these synthetic ids keep the simulator
# deterministic. message_variation_ids is the campaign's message variations
# (Braze fatal errors "Message Variant Unspecified" / "Invalid Message
# Variant" validate against it).
_CAMPAIGNS = [
    {
        "id": "cmp001",
        "name": "Welcome Email",
        "status": "Active",
        "channels": ["email"],
        "message_variation_ids": ["variant-1", "variant-2"],
    },
    {
        "id": "cmp002",
        "name": "Weekly Newsletter",
        "status": "Active",
        "channels": ["email", "android_push"],
        "message_variation_ids": ["variant-1", "variant-2"],
    },
]

# _seed_campaigns loads the seeded campaigns into the campaigns collection
# exactly once per instance so send endpoints can validate campaign ids
# against the store.
def _seed_campaigns():
    if store_kv_get("braze", "campaigns_seeded") == "yes":
        return
    store_kv_set("braze", "campaigns_seeded", "yes")
    cc = store_collection("campaigns")
    for c in _CAMPAIGNS:
        doc = {}
        for k in c:
            doc[k] = c[k]
        cc.insert(doc)

# _campaign looks up a seeded campaign by id in the campaigns collection.
def _campaign(campaign_id):
    if campaign_id == None or campaign_id == "":
        return None
    _seed_campaigns()
    cc = store_collection("campaigns")
    for c in cc.list():
        if c.get("id") == campaign_id:
            return c
    return None

# _to_int parses an int from a string; empty/invalid -> 0.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _num coerces a JSON-round-tripped number (int or float) to int.
def _num(v):
    if v == None:
        return 0
    if type(v) == "int":
        return v
    if type(v) == "float":
        return int(v)
    return _to_int(str(v))

# _list_page applies Braze-style cursor pagination to a list of docs via the
# paginate builtin. The `limit` query param sets the page size (a missing/empty
# value disables paging -> returns all items); the `cursor` query param is the
# opaque token returned by a prior call (None/"" for the first page). Returns
# (page, next_cursor) where next_cursor is the opaque token for the next page,
# or None when done.
def _list_page(req, docs):
    query = req.get("query", {})
    if query == None:
        query = {}
    limit = _to_int(query.get("limit", ""))
    cursor = query.get("cursor", "")
    if cursor == None:
        cursor = ""
    page, next_cursor = paginate(docs, limit, cursor)
    return page, next_cursor

# ============================================================================
# REQUEST BODY (authoritative raw_body + total JSON decode)
# ============================================================================

# _body_of returns (body, True) — a dict, empty when the request had no
# body — or (None, False) when the body is non-empty but not a JSON object.
# raw_body is the AUTHORITATIVE source: an undecodable body surfaces as an
# EMPTY DICT via req["body"] (a nil map converts to {}), so the raw bytes
# are always decoded first with json_safe_decode (callers answer 400, never
# a 500).
def _body_of(req):
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    if raw != "":
        decoded = json_safe_decode(raw)
        if decoded == None or type(decoded) != "dict":
            return None, False
        return decoded, True
    b = req.get("body")
    if b != None and type(b) == "dict":
        return b, True
    return {}, True

# _bad_body is the fatal response for an undecodable JSON body (Braze fatal
# error "Bad Request" / "Bad syntax.").
def _bad_body():
    return respond(400, {
        "message": "Bad Request",
        "errors": [{"Bad Request": "Bad syntax. Could not parse the request body as JSON."}],
    })

# ============================================================================
# FATAL ERRORS (Braze REST fatal-error vocabulary — see
# https://www.braze.com/docs/api/errors: the body is
# {"message": <fatal error message>, "errors": [<minor error messages>]}).
# ============================================================================

def _fatal(msg, detail):
    return respond(400, {
        "message": msg,
        "errors": [{msg: detail}],
    })

# ============================================================================
# IDS (real Braze shapes: dispatch_id = 32-char lowercase hex, braze_id =
# 24-char hex, schedule_id = UUID-shaped; assembled from KV counters at
# runtime so scripts carry no long digit literals).
# ============================================================================

def _to_hex(n):
    if n == 0:
        return "0"
    # Hex digits, assembled so no long digit run appears in a literal.
    digits = "0123" + "45" + "6789abcdef"
    s = ""
    while n > 0:
        s = digits[n % 16] + s
        n = n // 16
    return s

def _pad_to(s, w):
    while len(s) < w:
        s = "0" + s
    return s

# _next_dispatch_id mints a Braze dispatch id (32-char lowercase hex).
def _next_dispatch_id():
    return _pad_to(_to_hex(store_kv_incr("braze", "dispatch_seq")), 32)

# _next_braze_id mints a Braze device user id (24-char hex).
def _next_braze_id():
    return _pad_to(_to_hex(store_kv_incr("braze", "braze_id_seq")), 24)

# _next_schedule_id mints a UUID-shaped schedule id (8-4-4-4-12 hex).
def _next_schedule_id():
    h = _pad_to(_to_hex(store_kv_incr("braze", "schedule_seq")), 32)
    return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]

# ============================================================================
# ISO 8601 (schedule/event/purchase times; Starlark has no date builtin)
# ============================================================================

# _digits_val parses n digits at s[i:] -> (value, ok).
def _digits_val(s, i, n):
    v = 0
    if i + n > len(s):
        return 0, False
    for j in range(n):
        ch = s[i + j]
        if ch < "0" or ch > "9":
            return 0, False
        v = v * 10 + (ord(ch) - ord("0"))
    return v, True

_CUM_DAYS = [0, 31, 59, 90, 120, 151, 181, 212, 243, 273, 304, 334]

def _is_leap(y):
    if y % 4 == 0 and y % 100 != 0:
        return True
    return y % 400 == 0

def _days_from_1970(y, mo, d):
    days = (y - 1970) * 365
    lm1 = (y - 1) // 4 - (y - 1) // 100 + (y - 1) // 400
    l69 = 1969 // 4 - 1969 // 100 + 1969 // 400
    days = days + (lm1 - l69)
    days = days + _CUM_DAYS[mo - 1] + (d - 1)
    if mo > 2 and _is_leap(y):
        days = days + 1
    return days

# _parse_iso8601 parses "YYYY-MM-DDTHH:MM:SS[.fff][Z|+HH:MM|-HH:MM]" to unix
# seconds, or None when malformed (used for Braze's documented "Cannot parse
# send_at datetime." fatal error and for event/purchase time validation).
def _parse_iso8601(s):
    if s == None or type(s) != "string" or len(s) < 19:
        return None
    if s[4] != "-" or s[7] != "-" or s[13] != ":" or s[16] != ":":
        return None
    sep = s[10]
    if sep != "T" and sep != "t" and sep != " ":
        return None
    y, ok1 = _digits_val(s, 0, 4)
    mo, ok2 = _digits_val(s, 5, 2)
    d, ok3 = _digits_val(s, 8, 2)
    h, ok4 = _digits_val(s, 11, 2)
    mi, ok5 = _digits_val(s, 14, 2)
    sec, ok6 = _digits_val(s, 17, 2)
    if not (ok1 and ok2 and ok3 and ok4 and ok5 and ok6):
        return None
    if y < 1970 or y > 9999:
        return None
    if mo < 1 or mo > 12 or d < 1 or d > 31:
        return None
    if h > 23 or mi > 59 or sec > 59:
        return None
    i = 19
    if i < len(s) and s[i] == ".":
        i = i + 1
        while i < len(s) and s[i] >= "0" and s[i] <= "9":
            i = i + 1
    tz_off = 0
    if i < len(s):
        ch = s[i]
        if ch == "Z" or ch == "z":
            i = i + 1
        elif ch == "+" or ch == "-":
            sign = 1
            if ch == "-":
                sign = -1
            tzh, okA = _digits_val(s, i + 1, 2)
            if not okA or i + 3 >= len(s) or s[i + 3] != ":":
                return None
            tzm, okB = _digits_val(s, i + 4, 2)
            if not okB:
                return None
            if tzh > 23 or tzm > 59:
                return None
            tz_off = sign * (tzh * 3600 + tzm * 60)
            i = i + 6
        else:
            return None
    if i != len(s):
        return None
    return _days_from_1970(y, mo, d) * 24 * 3600 + h * 3600 + mi * 60 + sec - tz_off

# ============================================================================
# USER PROFILES (users collection)
# ============================================================================

# Reserved profile fields (set at the top level of the exported profile);
# every other attribute tracked via /users/track lands in custom_attributes.
_PROFILE_FIELDS = [
    "first_name", "last_name", "email", "dob", "home_city", "country",
    "language", "phone", "time_zone", "gender", "email_subscribe",
    "push_subscribe",
]

# _clean_alias normalizes a user alias object to {alias_name, alias_label}.
def _clean_alias(a):
    return {
        "alias_name": a.get("alias_name", ""),
        "alias_label": a.get("alias_label", ""),
    }

# _alias_key is the collection id of the alias-only profile an alias points
# at (external_id profiles are keyed by their external_id).
def _alias_key(a):
    label = a.get("alias_label", "")
    name = a.get("alias_name", "")
    if label == None:
        label = ""
    if name == None:
        name = ""
    return "alias:" + label + ":" + name

# _find_by_alias returns the profile carrying (alias_label, alias_name), or
# None.
def _find_by_alias(uc, alias):
    if alias == None or type(alias) != "dict":
        return None
    label = alias.get("alias_label", "")
    name = alias.get("alias_name", "")
    if label == None or name == None or label == "" or name == "":
        return None
    for u in uc.list():
        aliases = u.get("user_aliases", [])
        if aliases == None:
            aliases = []
        for al in aliases:
            if al.get("alias_label", "") == label and al.get("alias_name", "") == name:
                return u
    return None

# _new_profile builds a fresh profile doc (keyed by external_id, or by the
# alias when it is alias-only). created_at is ISO 8601 per the export field
# table; braze_id is minted at runtime.
def _new_profile(eid, alias):
    aliases = []
    if alias != None and type(alias) == "dict":
        aliases = [_clean_alias(alias)]
    key = eid
    if key == None or key == "":
        key = _alias_key(alias)
    return {
        "id": key,
        "external_id": eid,
        "braze_id": _next_braze_id(),
        "user_aliases": aliases,
        "created_at": clock.now_rfc3339(),
        "custom_attributes": {},
    }

# _find_by_field scans profiles for a field value match (braze_id, email,
# phone — the real identifiers that resolve but never create a profile on
# their own).
def _find_by_field(uc, field, value):
    if value == None or value == "":
        return None
    for u in uc.list():
        v = u.get(field, None)
        if v != None and v == value:
            return u
    return None

# _resolve_user returns the profile a track/alias/identify record points at
# (by external_id, user_alias, braze_id, email, or phone), or None when
# unidentifiable. Never creates; callers use _upsert_user when they want
# create-on-write.
def _resolve_user(uc, rec):
    eid = rec.get("external_id", None)
    if eid != None and eid != "":
        return uc.get(str(eid))
    alias = rec.get("user_alias", None)
    if alias != None and type(alias) == "dict":
        found = _find_by_alias(uc, alias)
        if found != None:
            return found
    found = _find_by_field(uc, "braze_id", rec.get("braze_id", None))
    if found != None:
        return found
    found = _find_by_field(uc, "email", rec.get("email", None))
    if found != None:
        return found
    found = _find_by_field(uc, "phone", rec.get("phone", None))
    if found != None:
        return found
    return None

# _upsert_user resolves the profile a record points at, creating it when
# missing (external_id profile, or alias-only profile for user_alias;
# braze_id/email/phone only ever resolve). Returns the profile doc, or None
# when the record carries no usable identifier.
def _upsert_user(uc, rec):
    eid = rec.get("external_id", None)
    alias = rec.get("user_alias", None)
    if eid != None and eid != "":
        eid = str(eid)
        existing = uc.get(eid)
        if existing != None:
            return existing
        doc = _new_profile(eid, None)
        uc.insert(doc)
        return doc
    if alias != None and type(alias) == "dict":
        if alias.get("alias_name", "") == None or alias.get("alias_label", "") == None:
            return None
        if alias.get("alias_name", "") == "" or alias.get("alias_label", "") == "":
            return None
        existing = _find_by_alias(uc, alias)
        if existing != None:
            return existing
        doc = _new_profile(None, alias)
        uc.insert(doc)
        return doc
    found = _resolve_user(uc, rec)
    if found != None:
        return found
    return None

# _track_target resolves the profile a /users/track record targets,
# honoring the real _update_existing_only flag: update-only records never
# create users, and unknown targets are skipped without an error. Returns
# None when the record must not be written.
def _track_target(uc, rec):
    uo = rec.get("_update_existing_only", False)
    if uo != None and uo:
        return _resolve_user(uc, rec)
    return _upsert_user(uc, rec)

# _apply_attributes merges a /users/track attributes record into a profile:
# reserved profile fields land at the top level, everything else in
# custom_attributes (new values overwrite old, like the real API).
def _apply_attributes(profile, rec):
    for k in rec:
        if k == "external_id" or k == "user_alias" or k == "braze_id" or k == "_update_existing_only":
            continue
        if k in _PROFILE_FIELDS:
            profile[k] = rec[k]
        else:
            ca = profile.get("custom_attributes", {})
            if ca == None:
                ca = {}
            ca[k] = rec[k]
            profile["custom_attributes"] = ca

# ============================================================================
# EVENTS / PURCHASES (raw docs in the events/purchases collections; exports
# aggregate them per profile)
# ============================================================================

# _user_occurrences returns the raw docs in `collection` whose _user is the
# profile's collection id.
def _user_occurrences(collection_name, profile_id):
    c = store_collection(collection_name)
    return query_select(c.list(), [["_user", "=", profile_id]])

# _aggregate_occurrences folds raw event/purchase docs into the exported
# shape [{name, first, last, count}] (first/last keep the client-supplied
# ISO 8601 time of the earliest/latest occurrence; count is the number of
# tracked occurrences, all time).
def _aggregate_occurrences(docs, name_key):
    agg = {}
    order = []
    for d in docs:
        nm = d.get(name_key, "")
        if nm == None or nm == "":
            continue
        t = _num(d.get("_t", 0))
        if nm not in agg:
            agg[nm] = {
                "name": nm,
                "_first": t,
                "_last": t,
                "_first_s": d.get("time", ""),
                "_last_s": d.get("time", ""),
                "count": 0,
            }
            order.append(nm)
        a = agg[nm]
        a["count"] = a["count"] + 1
        if t < a["_first"]:
            a["_first"] = t
            a["_first_s"] = d.get("time", "")
        if t > a["_last"]:
            a["_last"] = t
            a["_last_s"] = d.get("time", "")
    out = []
    for nm in order:
        a = agg[nm]
        out.append({
            "name": nm,
            "first": a["_first_s"],
            "last": a["_last_s"],
            "count": a["count"],
        })
    return out

# ============================================================================
# PROFILE EXPORT (POST /users/export/ids user object)
# ============================================================================

# _export_user builds the exported profile. fields is the fields_to_export
# list (None/empty = the entire profile, the real default for this
# endpoint). Braze includes the least data possible; unknown projection
# fields are omitted.
def _export_user(profile, fields):
    pid = profile.get("id", "")
    events = _aggregate_occurrences(_user_occurrences("events", pid), "name")
    purchases = _aggregate_occurrences(_user_occurrences("purchases", pid), "product_id")
    full = {
        "created_at": profile.get("created_at", None),
        "external_id": profile.get("external_id", None),
        "user_aliases": profile.get("user_aliases", []),
        "braze_id": profile.get("braze_id", None),
        "first_name": profile.get("first_name", None),
        "last_name": profile.get("last_name", None),
        "email": profile.get("email", None),
        "dob": profile.get("dob", None),
        "home_city": profile.get("home_city", None),
        "country": profile.get("country", None),
        "phone": profile.get("phone", None),
        "language": profile.get("language", None),
        "time_zone": profile.get("time_zone", None),
        "gender": profile.get("gender", None),
        "email_subscribe": profile.get("email_subscribe", None),
        "push_subscribe": profile.get("push_subscribe", None),
        "custom_attributes": profile.get("custom_attributes", {}),
        "custom_events": events,
        "purchases": purchases,
    }
    if fields == None or len(fields) == 0:
        return full
    out = {}
    for f in fields:
        if f in full:
            out[f] = full[f]
    return out

# ============================================================================
# OUTBOUND WEBHOOKS (UNSIGNED BY DESIGN — DOCUMENTATION)
# ============================================================================
# Braze's "Webhooks" messaging channel sends arbitrary HTTP requests whose
# method, headers, and body the CUSTOMER configures per message; Braze applies
# no provider-side signature to those deliveries (event analytics streaming
# goes through Braze Currents to S3/Azure/Data warehouse, not webhooks). There
# is consequently no documented outbound signature scheme to reproduce, and
# this adapter emits UNSIGNED deliveries with a synthetic payload shape.
#
# Receivers that need authentication should treat these like a Braze webhook
# configured with a customer-supplied Authorization header — which, by
# design, this simulator does not add.
# ============================================================================

# _emit_if_subscribed delivers an unsigned event to the registered webhook
# target when a hook subscribes to the event type (empty events list
# subscribes to everything). No delivery when no webhook is registered.
# Callers persist the affected docs BEFORE calling this so an emitted event
# always reflects stored state.
def _emit_if_subscribed(event_type, payload):
    wc = store_collection("webhooks")
    for h in wc.list():
        evts = h.get("events", [])
        if evts == None:
            evts = []
        if len(evts) == 0 or event_type in evts:
            events_emit(event_type, payload)
            return

# ============================================================================
# DISPATCHES (dispatches collection) — every send/scheduled-send records a
# dispatch doc with the real Braze dispatch id BEFORE its webhook is emitted.
# ============================================================================

def _record_dispatch(dispatch_id, campaign_id, schedule_id, channels, recipients, status):
    dc = store_collection("dispatches")
    doc = {
        "id": dispatch_id,
        "campaign_id": campaign_id,
        "schedule_id": schedule_id,
        "channels": channels,
        "recipients": recipients,
        "status": status,
        "created_at": clock.now_rfc3339(),
    }
    dc.insert(doc)
