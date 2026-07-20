# Verify handlers — Twilio Verify v2 verification + check.
#
# POST /v2/Services/{service_sid}/Verification
#   JSON { To, Channel:"sms" } -> { status:"pending", sid:"VL..." }
# POST /v2/Services/{service_sid}/VerificationCheck
#   JSON { To, Code } -> { status:"approved" } if code matches, else "pending"
#
# The generated code is deterministic for local testing: the last 6 digits
# of the To phone number (zero-padded). This lets developers write realistic
# verification round-trip tests without external state.

# Shared helpers (_require_auth, _next_sid) are preloaded from
# scripts/lib.star.

# _gen_code generates a deterministic verification code from a phone number.
# Takes the last 6 digits of the digits in the To field, zero-padded to 6.
def _gen_code(to):
    digits = ""
    for i in range(len(to)):
        ch = to[i]
        if ch >= "0" and ch <= "9":
            digits = digits + ch
    if len(digits) < 6:
        # Pad with leading zeros if too short.
        while len(digits) < 6:
            digits = "0" + digits
        return digits
    return digits[len(digits) - 6:]

# on_create_verification starts a verification.
def on_create_verification(req):
    err = _require_auth(req)
    if err != None:
        return err

    service_sid = req["params"]["service_sid"]

    body = req["body"]
    if body == None:
        body = {}

    to = body.get("To", "")
    if to == None:
        to = ""
    channel = body.get("Channel", "sms")
    if channel == None:
        channel = "sms"

    sid = _next_sid("VL")
    code = _gen_code(to)

    verification = {
        "sid": sid,
        "service_sid": service_sid,
        "account_sid": ACCOUNT_SID,
        "to": to,
        "channel": channel,
        "status": "pending",
        "valid": False,
        "lookup": {},
        "amount": None,
        "payee": None,
        "date_created": "Mon, 01 Jan 2024 00:00:00 +0000",
        "date_updated": "Mon, 01 Jan 2024 00:00:00 +0000",
        "send_code_attempts": 1,
        "sna": None,
        "url": "https://verify.stunt.local/v2/Services/" + service_sid + "/Verifications/" + sid,
    }

    # Store verification with the expected code for later checking.
    stored = {}
    for k in verification:
        stored[k] = verification[k]
    stored["code"] = code
    stored["id"] = sid
    c = store_collection("verifications")
    c.insert(stored)

    return respond(201, verification)

# on_check_verification checks a verification code.
def on_check_verification(req):
    err = _require_auth(req)
    if err != None:
        return err

    service_sid = req["params"]["service_sid"]

    body = req["body"]
    if body == None:
        body = {}

    to = body.get("To", "")
    if to == None:
        to = ""
    code = body.get("Code", "")
    if code == None:
        code = ""

    expected_code = _gen_code(to)

    c = store_collection("verifications")
    all_verifs = c.list()

    # Find the most recent verification for this "To" (pending or approved).
    found = None
    for v in all_verifs:
        if v.get("to", "") != to:
            continue
        if v.get("service_sid", "") != service_sid:
            continue
        found = v

    if found == None:
        # No pending verification — return a pending check result.
        return respond(404, {
            "code": 20404,
            "message": "The requested resource was not found",
            "more_info": "https://www.twilio.com/docs/errors/20404",
            "status": 404,
        })

    if code == expected_code:
        found["status"] = "approved"
        found["valid"] = True
        c.update(found["id"], found)
        result = {}
        for k in found:
            if k != "code":
                result[k] = found[k]
        return respond(200, result)

    # Wrong code — keep status pending, return the check result.
    found["send_code_attempts"] = found.get("send_code_attempts", 1) + 1
    c.update(found["id"], found)
    result = {}
    for k in found:
        if k != "code":
            result[k] = found[k]
    return respond(200, result)
