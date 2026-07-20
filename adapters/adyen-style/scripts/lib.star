# Shared library for adyen-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# Adyen Checkout API uses the X-API-Key header for authentication.
# We check presence only — the value is not validated against real Adyen
# credentials.

# _require_apikey validates that X-API-Key is present. Returns None if
# authorized, or an error-response dict if not.
def _require_apikey(req):
    headers = req.get("headers")
    if headers == None:
        return _adyen_err(401, "401", "Unauthorized", "security")
    # Go's net/http canonicalizes header names, so "X-API-Key" becomes
    # "X-Api-Key". Try both forms for robustness.
    apikey = headers.get("X-Api-Key", headers.get("X-API-Key", ""))
    if apikey == None or apikey == "":
        return _adyen_err(401, "401", "Unauthorized", "security")
    return None

# _adyen_err returns an Adyen-style error response.
# Shape: { status, errorCode, message, errorType }
def _adyen_err(status, errorCode, message, errorType):
    return respond(status, {
        "status": status,
        "errorCode": errorCode,
        "message": message,
        "errorType": errorType,
    })

# _psp_reference generates a PSP reference from the sequence counter.
def _psp_reference():
    n = store_kv_incr("adyen", "psp_seq")
    return "881" + str(4000000000000 + n)

# _mod_psp_reference generates a modification PSP reference.
def _mod_psp_reference(prefix):
    n = store_kv_incr("adyen", "mod_seq")
    return prefix + str(8000000000000 + n)

# _determine_result_code models Adyen's deterministic test outcomes.
#
# Adyen test card numbers:
#   4111... → Authorised
#   4000...0002 → Refused (generic refused)
#   4000...0069 → Received (authorized but requires additional action)
#
# We keep it simple and deterministic for testing.
def _determine_result_code(payment_method):
    number = ""
    if payment_method != None:
        number = payment_method.get("number", "")
        if number == None:
            number = ""

    # Refused test card.
    if _ends_with(number, "0002"):
        return "Refused"

    # Received (requires action).
    if _ends_with(number, "0069"):
        return "Received"

    # Default: Authorised.
    return "Authorised"

# _ends_with checks if str s ends with suffix.
def _ends_with(s, suffix):
    if len(s) < len(suffix):
        return False
    return s[-len(suffix):] == suffix

# _card_summary returns the last 4 digits of a card number.
def _card_summary(number):
    if number == None or len(number) < 4:
        return "1111"
    return number[-4:]

# _payment_public returns the Adyen-shaped payment response.
def _payment_public(doc):
    result_code = doc.get("resultCode", "Authorised")
    result = {
        "pspReference": doc["id"],
        "resultCode": result_code,
        "additionalData": doc.get("additionalData", {}),
    }

    # For Refused, include refusalReason.
    if result_code == "Refused":
        result["refusalReason"] = doc.get("refusalReason", "Refused")

    # For Received, include action.
    if result_code == "Received":
        result["action"] = doc.get("action", {
            "type": "threeDS2",
            "paymentData": doc["id"],
        })

    return result

# _modification_public returns the Adyen-shaped modification response.
# Shape: { pspReference, status:"received", paymentPspReference }
def _modification_public(mod_psp, payment_psp):
    return {
        "pspReference": mod_psp,
        "status": "received",
        "paymentPspReference": payment_psp,
    }
