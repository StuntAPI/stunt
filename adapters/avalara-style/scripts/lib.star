# Shared library for avalara-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support).

# Avalara auth: Bearer (account/license key) OR HTTP Basic.
# We check presence of either.

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

# _require_auth validates the auth (Bearer or Basic). Returns None if
# authorized, or an error-response dict if not.
def _require_auth(req):
    token = _bearer(req)
    if token != "":
        return None
    # Check for Basic auth.
    headers = req.get("headers")
    if headers != None:
        auth = headers.get("Authorization", "")
        if auth != None and auth.startswith("Basic "):
            return None
    return _avalara_err(401, "AuthenticationRequired", "Missing or invalid authentication credentials")

# _avalara_err returns an Avalara-style error response.
# Shape: { error: { code, message, target, details: [...] } }
def _avalara_err(status, code, message, target=""):
    return respond(status, {
        "error": {
            "code": code,
            "message": message,
            "target": target,
            "details": [],
        },
    })

# _txn_id generates an AvaTax transaction ID.
def _txn_id():
    n = store_kv_incr("avalara", "txn_seq")
    return str(3000000000 + n)

# _txn_code generates an AvaTax transaction code (companyCode-documentCode).
def _txn_code():
    n = store_kv_incr("avalara", "code_seq")
    return "INV-" + str(n)

# _company_id generates an AvaTax company ID.
def _company_id():
    n = store_kv_incr("avalara", "company_seq")
    return str(2000000000 + n)

# --- Tax calculation helpers ---

# _effective_rate returns a fixed effective tax rate based on the address state.
# This is synthetic — real AvaTax uses geocoded jurisdiction data.
def _effective_rate(state):
    rates = {
        "CA": 0.0950,
        "NY": 0.0875,
        "TX": 0.0825,
        "WA": 0.0980,
        "FL": 0.0700,
    }
    if state == None:
        state = ""
    rate = rates.get(state)
    if rate == None:
        return 0.0825  # default
    return rate

# _jurisdiction_breakdown splits an effective rate into state/county/city/special.
# The split is deterministic and sums to the effective rate.
def _jurisdiction_breakdown(state, effective_rate):
    if state == None:
        state = ""
    # State gets ~50%, county ~25%, city ~20%, special ~5%.
    state_rate = _round4(effective_rate * 0.50)
    county_rate = _round4(effective_rate * 0.25)
    city_rate = _round4(effective_rate * 0.20)
    special_rate = _round4(effective_rate - state_rate - county_rate - city_rate)

    county_name = state + " County"
    city_name = state + " City"

    return [
        {
            "jurisdiction": state,
            "jurisdictionType": "State",
            "rate": state_rate,
            "tax": "tax_placeholder",
            "taxName": state + " State Tax",
        },
        {
            "jurisdiction": county_name,
            "jurisdictionType": "County",
            "rate": county_rate,
            "tax": "tax_placeholder",
            "taxName": county_name + " Tax",
        },
        {
            "jurisdiction": city_name,
            "jurisdictionType": "City",
            "rate": city_rate,
            "tax": "tax_placeholder",
            "taxName": city_name + " Tax",
        },
        {
            "jurisdiction": "Special",
            "jurisdictionType": "Special",
            "rate": special_rate,
            "tax": "tax_placeholder",
            "taxName": "Special District Tax",
        },
    ]

# _compute_tax computes tax for a list of line amounts and an address.
# Returns: { totalTax, totalTaxable, lines: [{ number, tax, taxCalculated, details }] }
def _compute_tax(lines, state):
    # Determine effective rate from the address.
    rate = _effective_rate(state)

    # Build jurisdiction breakdown (rates only — tax computed per line).
    breakdown = _jurisdiction_breakdown(state, rate)

    total_tax = 0.0
    total_taxable = 0.0
    result_lines = []

    for line_in in lines:
        if line_in == None:
            continue
        amount = _to_float(line_in.get("amount", 0))
        if amount == None:
            amount = 0.0
        number = line_in.get("number", "1")
        if number == None:
            number = "1"

        tax = _round2(amount * rate)
        total_tax = total_tax + tax
        total_taxable = total_taxable + amount

        # Build per-jurisdiction tax details.
        details = []
        for bd in breakdown:
            details.append({
                "jurisdiction": bd["jurisdiction"],
                "jurisdictionType": bd["jurisdictionType"],
                "rate": bd["rate"],
                "tax": _round2(amount * bd["rate"]),
                "taxName": bd["taxName"],
            })

        result_lines.append({
            "number": number,
            "tax": _fmt(tax),
            "taxCalculated": _fmt(tax),
            "taxCode": line_in.get("taxCode", "P0000000"),
            "details": details,
        })

    # Build summary.
    summary = []
    for bd in breakdown:
        summary.append({
            "jurisCode": bd["jurisdiction"][:10],
            "jurisName": bd["jurisdiction"],
            "jurisdictionType": bd["jurisdictionType"],
            "taxType": "Sales",
            "rate": bd["rate"],
            "tax": _fmt(total_taxable * bd["rate"]),
            "taxName": bd["taxName"],
        })

    return {
        "totalTax": _fmt(total_tax),
        "totalTaxable": _fmt(total_taxable),
        "totalRate": rate,
        "lines": result_lines,
        "summary": summary,
    }

# _to_float converts a value to float64 (handles int, string, float).
def _to_float(val):
    if val == None:
        return 0.0
    if type(val) == "int":
        return float(val)
    if type(val) == "float":
        return val
    # String.
    return 0.0

# _round2 rounds a float to 2 decimal places.
def _round2(val):
    # Starlark has no round() builtin; use int() truncation after offset.
    return float(int(val * 100 + 0.5)) / 100.0

# _round4 rounds a float to 4 decimal places.
def _round4(val):
    return float(int(val * 10000 + 0.5)) / 10000.0

# _fmt formats a float as a string like "8.25".
def _fmt(val):
    return str(val)

# _address_state extracts the state/region from an addresses structure.
def _address_state(addresses):
    if addresses == None:
        return ""
    # singleLocation form.
    sl = addresses.get("singleLocation")
    if sl != None:
        return sl.get("region", sl.get("state", ""))
    # shipFrom + shipTo form.
    ship_to = addresses.get("shipTo")
    if ship_to != None:
        return ship_to.get("region", ship_to.get("state", ""))
    ship_from = addresses.get("shipFrom")
    if ship_from != None:
        return ship_from.get("region", ship_from.get("state", ""))
    return ""
