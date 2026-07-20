# Tax calculation handler — the quick tax estimate.
#
# Requires auth.
# POST /v2/tax/calculate ({addresses, lines}) → { totalTax, totalTaxable, lines, summary }

# on_calculate_tax computes a quick tax estimate.
def on_calculate_tax(req):
    err = _require_auth(req)
    if err != None:
        return err

    body = req.get("body")
    if body == None:
        body = {}

    addresses = body.get("addresses", {})
    if addresses == None:
        addresses = {}

    lines = body.get("lines", [])
    if lines == None:
        lines = []

    state = _address_state(addresses)
    result = _compute_tax(lines, state)

    return respond(200, {
        "totalTax": result["totalTax"],
        "totalTaxable": result["totalTaxable"],
        "totalRate": result["totalRate"],
        "lines": result["lines"],
        "summary": result["summary"],
    })
