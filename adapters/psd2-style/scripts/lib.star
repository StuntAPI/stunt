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
# Checks that a bearer token is present.
# Returns None if authorized, or an error-response dict if not.
def _require_tpp(req):
    token = _bearer(req)
    if token == "":
        return _psd2_err(401, "ERROR", "CONSENT_INVALID", "Missing or invalid access token")
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
