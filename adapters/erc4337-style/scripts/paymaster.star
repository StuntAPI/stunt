# Paymaster handler — mock paymaster sponsorship signing.
#
# POST /paymaster/sign ({userOp}) → returns updated paymasterAndData with
# a synthetic signature. Models a verifying paymaster that sponsors a
# userOperation.

# Shared helpers are preloaded from scripts/lib.star.

# The mock paymaster address (synthetic).
PAYMASTER_ADDRESS = "0x0000000000000000000000000000000000000001"

# on_sign returns a signed paymasterAndData for the given userOp.
def on_sign(req):
    body = req.get("body")
    if body == None:
        body = {}

    userop = body.get("userOp", None)
    if userop == None:
        userop = body.get("userOperation", None)
    if userop == None:
        return respond(400, {"error": "missing_userOp", "message": "userOp is required"})

    # Validate the userOp.
    err = _validate_userop(userop)
    if err != None:
        return respond(400, {"error": "invalid_userOp", "message": err})

    # Compute a deterministic paymaster signature from the userOp.
    sender = userop.get("sender", "")
    nonce = userop.get("nonce", "0x0")
    call_data = userop.get("callData", "0x")
    sig = _deterministic_hash("paymaster_" + sender + str(nonce) + call_data)

    # paymasterAndData = paymasterAddress (20 bytes) + validUntil (32 bytes) + validAfter (32 bytes) + signature (variable)
    # We model it as: address + hex signature (deterministic).
    paymaster_and_data = PAYMASTER_ADDRESS + sig[2:]

    return respond(200, {
        "paymasterAndData": paymaster_and_data,
        "validUntil": "0x0",
        "validAfter": "0x0",
    })
