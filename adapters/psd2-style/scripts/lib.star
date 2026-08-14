# Shared library for psd2-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# PSD2 NextGenPSD2 uses OAuth2 bearer tokens for TPP authentication.
# Account access additionally requires a valid consent.

# _bearer extracts the Bearer token from the Authorization header.
def _bearer(req):
    headers = req.get("headers")
    if headers == None:
        return ""
    auth = headers.get("Authorization", "")
    if auth == None:
        auth = ""
    if auth.startswith("Bearer "):
        return auth[7:]
    return ""

# _get_query safely returns the request query-string dict.
def _get_query(req):
    q = req.get("query")
    if q == None:
        return {}
    return q

# _to_int parses a loose query-string value into an int.
# Returns 0 for None / "" / non-numeric strings.
def _to_int(v):
    if v == None:
        return 0
    s = str(v)
    if s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# === List pagination ===

# _list_page applies Berlin Group NextGenPSD2 paging to a full list of
# resources. It reads the provider's size (page size) and page (cursor, an
# opaque offset token carried via _links.next.href) query params and delegates
# to the pure paginate() builtin.
#
# Returns (page, next_cursor) where next_cursor is an opaque string token for
# the next page, or None when no items remain. When size is absent or <= 0
# paging is disabled and the whole list is returned with next_cursor None,
# preserving the unpaginated behavior.
def _list_page(req, docs):
    q = _get_query(req)
    limit = _to_int(q.get("size", ""))
    cursor = q.get("page", "")
    if cursor == None:
        cursor = ""
    return paginate(docs, limit, cursor)

# _page_links returns the _links block additions for paginated list responses.
# When next_cursor is not None it adds a "next" link with the cursor encoded as
# the page query param, matching the NextGenPSD2 _links.next.href convention.
# size_hint is the page size echoed back into the next href (string).
def _page_links(base_href, next_cursor, size_hint):
    if next_cursor == None or next_cursor == "":
        return {}
    return {
        "next": {"href": base_href + "?page=" + next_cursor + "&size=" + size_hint},
    }

# _psd2_err returns a NextGenPSD2-style error response.
# Shape: { tppMessages: [{ category, code, text }] }
def _psd2_err(status, category, code, text):
    return respond(status, {
        "tppMessages": [
            {
                "category": category,
                "code": code,
                "text": text,
            },
        ],
    })

# _consent_id generates a consent ID.
def _consent_id():
    n = store_kv_incr("psd2", "consent_seq")
    return "consent-" + str(7000000000 + n)

# _authorisation_id generates an authorisation ID.
def _authorisation_id():
    n = store_kv_incr("psd2", "auth_seq")
    return "auth-" + str(8000000000 + n)

# _require_tpp validates the bearer token (TPP-level auth).
# The token must be one minted by /v1/oauth/token (stored in the
# access_tokens collection with an expires_at unix timestamp) and unexpired.
# Unknown or expired tokens get the NextGenPSD2 401 tppMessages envelope.
# Returns None if authorized, or an error-response dict if not.
def _require_tpp(req):
    token = _bearer(req)
    if token == "":
        return _psd2_err(401, "ERROR", "TOKEN_INVALID", "Missing or invalid access token")

    tc = store_collection("access_tokens")
    doc = tc.get(token)
    if doc == None:
        return _psd2_err(401, "ERROR", "TOKEN_INVALID", "Missing or invalid access token")

    expires_at = doc.get("expires_at", 0)
    if type(expires_at) != "int":
        expires_at = _to_int(str(expires_at))
    if expires_at > 0 and clock.now_unix() > expires_at:
        return _psd2_err(401, "ERROR", "TOKEN_EXPIRED", "The access token has expired")

    return None

# _require_consent validates the bearer token AND checks that at least one
# valid consent exists. Returns None if authorized, or an error-response.
def _require_consent(req):
    err = _require_tpp(req)
    if err != None:
        return err

    # Check for at least one valid consent.
    cc = store_collection("consents")
    all_consents = cc.list()
    has_valid = False
    for c in all_consents:
        if c.get("consentStatus", "") == "valid":
            has_valid = True
            break

    if not has_valid:
        return _psd2_err(401, "ERROR", "CONSENT_INVALID", "No valid consent found")

    return None

# _consent_public returns the NextGenPSD2-shaped consent object with _links.
def _consent_public(doc):
    consent_id = doc["id"]
    status = doc.get("consentStatus", "received")

    links = {
        "self": {"href": "https://api.stunt.test/v1/consents/" + consent_id},
    }

    if status == "received":
        links["startAuthorisation"] = {"href": "https://api.stunt.test/v1/consents/" + consent_id + "/authorisations"}
    if status == "valid":
        links["status"] = {"href": "https://api.stunt.test/v1/consents/" + consent_id}
    links["scaStatus"] = {"href": "https://api.stunt.test/v1/consents/" + consent_id + "/authorisations"}

    return {
        "consentId": consent_id,
        "consentStatus": status,
        "access": doc.get("access", {}),
        "recurringIndicator": doc.get("recurringIndicator", True),
        "validUntil": doc.get("validUntil", ""),
        "frequencyPerDay": doc.get("frequencyPerDay", 4),
        "lastActionDate": doc.get("lastActionDate", "2024-01-01"),
        "_links": links,
    }

# _authorisation_public returns the authorisation object with _links.
def _authorisation_public(doc):
    consent_id = doc.get("consentId", "")
    auth_id = doc["id"]
    sca_status = doc.get("scaStatus", "started")

    links = {
        "scaStatus": {"href": "https://api.stunt.test/v1/consents/" + consent_id + "/authorisations/" + auth_id},
        "self": {"href": "https://api.stunt.test/v1/consents/" + consent_id + "/authorisations/" + auth_id},
    }

    # When SCA is started, include the redirect link to the bank's SCA page.
    if sca_status == "started" or sca_status == "psuAuthenticated":
        links["scaRedirect"] = {
            "href": "https://bank.stunt.test/sca/redirect?consent=" + consent_id + "&auth=" + auth_id,
        }

    return {
        "authorisationId": auth_id,
        "scaStatus": sca_status,
        "consentId": consent_id,
        "authenticationMethodId": doc.get("authenticationMethodId", ""),
        "_links": links,
    }
