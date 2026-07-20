# Phone number handlers — registration status + register number.
#
# The phone number GET is handled by the generic resource handler
# (resource.star#on_get_resource) when the resource_id is a phone number ID.
#
# POST /v21.0/{phone_number_id}/register → {success: true}
#
# Requires Bearer access token.

# Shared helpers (_require_auth, _wa_unauthorized, _now, _seed) are preloaded
# from scripts/lib.star.

# on_register registers a phone number (2FA PIN verification).
def on_register(req):
    err = _require_auth(req)
    if err != None:
        return err
    _seed()

    phone_number_id = req["params"]["phone_number_id"]
    body = req["body"]
    if body == None:
        body = {}

    # Update the phone number record to registered status.
    pc = store_collection("phone_numbers")
    phone = pc.get(phone_number_id)
    if phone != None:
        phone["code_verification_status"] = "VERIFIED"
        pc.update(phone_number_id, phone)

    return respond(200, {"success": True})
