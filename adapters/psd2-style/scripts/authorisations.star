# Authorisation handlers — the SCA (Strong Customer Authentication) flow.
#
# This is the core PSD2 pain point: the PSU (end-user) must authenticate via
# the bank's SCA page. The flow is a STAGED chain (no jumping):
#
#   started → psuAuthenticated → scaReceived → finalised
#
#   POST /v1/consents/{consentId}/authorisations
#       → { authorisationId, scaStatus:"started", _links:{ scaRedirect } }
#   GET  /v1/consents/{consentId}/authorisations/{authorisationId}
#       → { scaStatus, ... }  (derive-on-read: scaReceived finalises once
#                              the challenge window elapses)
#   PUT  /v1/consents/{consentId}/authorisations/{authorisationId}
#       → one hop per call: authenticationMethodId → psuAuthenticated,
#         scaAuthenticationData → scaReceived. The consent becomes "valid"
#         only when the authorisation finalises (derive-on-read).
#
# Shared helpers (_load_authorisation, _sca_get, _sca_update, _advance_auth,
# _signed_emit) are preloaded from scripts/lib.star; payment authorisations
# (scripts/payments.star) run the exact same chain.

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
        "resourceType": "consent",
        "resourceId": consent_id,
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

# on_get_authorisation retrieves the SCA status of an authorisation,
# advancing the derive-on-read chain first.
def on_get_authorisation(req):
    err = _require_tpp(req)
    if err != None:
        return err
    return _sca_get(req, "consent")

# on_update_authorisation advances the SCA chain one hop (method selection
# or OTP submission). Finalisation — and the consent becoming "valid" — is
# derive-on-read once the challenge window elapses.
def on_update_authorisation(req):
    err = _require_tpp(req)
    if err != None:
        return err
    return _sca_update(req, "consent")
