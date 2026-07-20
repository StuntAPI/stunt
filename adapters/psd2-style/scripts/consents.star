# Consent handlers — create, get, delete.
#
# STATEFUL lifecycle: received → valid (after SCA finalisation)
#
# POST   /v1/consents          → { consentId, consentStatus:"received", _links }
# GET    /v1/consents/{id}     → { consentId, consentStatus, _links }
# DELETE /v1/consents/{id}     → { consentStatus:"terminated" }

# on_create_consent creates a new PSD2 consent.
def on_create_consent(req):
    err = _require_tpp(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    access = body.get("access", {})
    if access == None:
        access = {}
    recurring_indicator = body.get("recurringIndicator", True)
    valid_until = body.get("validUntil", "2025-12-31")
    frequency_per_day = body.get("frequencyPerDay", 4)
    combined_service_indicator = body.get("combinedServiceIndicator", False)

    consent_id = _consent_id()

    doc = {
        "id": consent_id,
        "consentStatus": "received",
        "access": access,
        "recurringIndicator": recurring_indicator,
        "validUntil": valid_until,
        "frequencyPerDay": frequency_per_day,
        "combinedServiceIndicator": combined_service_indicator,
        "lastActionDate": "2024-01-01",
        "authorisationId": "",
    }

    c = store_collection("consents")
    c.insert(doc)

    return respond(201, _consent_public(doc))

# on_get_consent retrieves a consent by ID.
def on_get_consent(req):
    err = _require_tpp(req)
    if err != None:
        return err

    consent_id = req["params"]["consentId"]
    c = store_collection("consents")
    doc = c.get(consent_id)
    if doc == None:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Consent not found")

    return respond(200, _consent_public(doc))

# on_delete_consent terminates a consent.
def on_delete_consent(req):
    err = _require_tpp(req)
    if err != None:
        return err

    consent_id = req["params"]["consentId"]
    c = store_collection("consents")
    doc = c.get(consent_id)
    if doc == None:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Consent not found")

    doc["consentStatus"] = "terminated"
    c.update(consent_id, doc)

    return respond(200, _consent_public(doc))
