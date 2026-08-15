# Token handlers.
#
# POST /v1/tokens with a card dict creates a real Stripe-style card token
# (tok_*) whose stored card number drives the decline/SCA test-card behavior
# at PaymentIntent confirm and charge create (see lib.star). Without a card it
# remains the test helper that mints a real identity token for authenticated
# requests.

# _token_brand guesses the card brand from the number prefix.
def _token_brand(number):
    if number.startswith("4"):
        return "visa"
    pfx = number[:2]
    if pfx == "34" or pfx == "37":
        return "amex"
    if pfx == "51" or pfx == "52" or pfx == "53" or pfx == "54" or pfx == "55":
        return "mastercard"
    if pfx == "60" or pfx == "65":
        return "discover"
    return "visa"

# _digits strips non-digit characters from a card number.
def _digits(s):
    if s == None:
        return ""
    out = ""
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            out = out + ch
    return out

# _create_card_token validates + stores a card token. The full number is kept
# in the private _number field (never returned); the public token object
# carries only brand/last4/expiry like the real API.
def _create_card_token(card):
    number = _digits(card.get("number", ""))
    if number == "":
        return respond(400, {"error": {"message": "You must provide a number for the card.", "param": "card[number]", "type": "invalid_request_error"}})
    exp_month = card.get("exp_month", 12)
    exp_year = card.get("exp_year", 2030)

    tok = _next_id("tok")
    card_pub = {
        "id": _next_id("card"),
        "object": "card",
        "brand": _token_brand(number),
        "last4": number[len(number) - 4:],
        "exp_month": exp_month,
        "exp_year": exp_year,
        "funding": "credit",
        "country": "US",
        "name": card.get("name", None),
        "cvc_check": "unchecked",
    }
    doc = {
        "id": tok,
        "object": "token",
        "type": "card",
        "card": card_pub,
        "created": clock.now_unix(),
        "livemode": False,
        "used": False,
        "client_ip": None,
        "_number": number,
    }
    store_collection("tokens").insert(doc)
    return respond(201, {
        "id": tok,
        "object": "token",
        "type": "card",
        "card": card_pub,
        "created": doc["created"],
        "livemode": False,
        "used": False,
        "client_ip": None,
    })

# POST /v1/tokens — create a card token (card in body, auth required) or mint
# an identity test token (no auth).
def on_mint_token(req):
    body = req["body"]
    if body == None:
        body = {}
    card = body.get("card", None)
    if card != None and type(card) == "dict":
        err = _require_auth(req)
        if err != None:
            return err
        return _create_card_token(card)

    # Identity-token mint (test helper): accept optional subject/scopes from
    # the body, defaulting to test_user.
    subject = body.get("subject", "test_user")
    scopes = body.get("scopes", ["write"])

    token = identity_mint(subject, scopes)
    return respond(201, {"token": token})
