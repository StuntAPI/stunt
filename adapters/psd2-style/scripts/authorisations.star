# Authorisation handlers — the SCA (Strong Customer Authentication) flow.
#
# This is the core PSD2 pain point: the PSU (end-user) must authenticate via
# the bank's SCA page. The flow:
#
#   started → psuAuthenticated → finalised
#
# POST /v1/consents/{consentId}/authorisations
#     → { authorisationId, scaStatus:"started", _links:{ scaRedirect } }
# GET  /v1/consents/{consentId}/authorisations/{authorisationId}
#     → { scaStatus, authenticationMethodId, _links }
# PUT  /v1/consents/{consentId}/authorisations/{authorisationId}
#     → { scaStatus:"finalised" }  (consent becomes valid)

# on_start_authorisation begins the SCA flow for a consent.
def on_start_authorisation(req):
    err = _require_tpp(req)
    if err != None:
        return err

    consent_id = req["params"]["consentId"]
    cc = store_collection("consents")
    consent = cc.get(consent_id)
    if consent == None:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Consent not found")

    auth_id = _authorisation_id()

    doc = {
        "id": auth_id,
        "consentId": consent_id,
        "scaStatus": "started",
        "authenticationMethodId": "",
        "scaMethods": [
            {
                "authenticationType": "SMS_OTP",
                "authenticationMethodId": "901",
                "name": "SMS OTP",
            },
            {
                "authenticationType": "APP_OTP",
                "authenticationMethodId": "902",
                "name": "App OTP",
            },
        ],
    }

    ac = store_collection("authorisations")
    ac.insert(doc)

    # Link the consent to this authorisation.
    consent["authorisationId"] = auth_id
    cc.update(consent_id, consent)

    return respond(201, _authorisation_public(doc))

# on_get_authorisation retrieves the SCA status of an authorisation.
def on_get_authorisation(req):
    err = _require_tpp(req)
    if err != None:
        return err

    consent_id = req["params"]["consentId"]
    auth_id = req["params"]["authorisationId"]

    ac = store_collection("authorisations")
    doc = ac.get(auth_id)
    if doc == None:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation not found")

    if doc.get("consentId", "") != consent_id:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation does not belong to this consent")

    return respond(200, _authorisation_public(doc))

# on_update_authorisation updates the SCA authentication data.
# This simulates the PSU completing the SCA challenge, finalising the
# authorisation and making the consent valid.
def on_update_authorisation(req):
    err = _require_tpp(req)
    if err != None:
        return err

    consent_id = req["params"]["consentId"]
    auth_id = req["params"]["authorisationId"]

    ac = store_collection("authorisations")
    doc = ac.get(auth_id)
    if doc == None:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation not found")

    if doc.get("consentId", "") != consent_id:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Authorisation does not belong to this consent")

    body = req["body"]
    if body == None:
        body = {}

    auth_method_id = body.get("authenticationMethodId", "901")
    sca_auth_data = body.get("scaAuthenticationData", "")

    # Transition SCA status to finalised.
    doc["scaStatus"] = "finalised"
    doc["authenticationMethodId"] = auth_method_id
    ac.update(auth_id, doc)

    # Mark the consent as valid.
    cc = store_collection("consents")
    consent = cc.get(consent_id)
    if consent != None:
        consent["consentStatus"] = "valid"
        cc.update(consent_id, consent)

    # Emit consent status change event.
    events_emit("consent.status.changed", {
        "consentId": consent_id,
        "consentStatus": "valid",
        "scaStatus": "finalised",
    })

    return respond(200, _authorisation_public(doc))
