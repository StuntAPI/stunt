# Payment methods handler — Adyen Checkout /paymentMethods.
#
# POST /v68/paymentMethods
#   { "merchantAccount": "...", "amount": {...}, "countryCode": "NL" }
#     → { "paymentMethods": [ { "type": "scheme", "name": "Credit Card",
#         "brands": [...] }, ... ], "storedPaymentMethods": [] }
#
# The real endpoint returns the payment methods configured for the merchant
# account (filtered by country/amount/currency). The simulator returns a
# fixed catalogue covering the methods the other endpoints model, and an
# empty storedPaymentMethods list (no recurring details are stored).

# Shared helpers (_require_apikey) are preloaded from scripts/lib.star.

def on_payment_methods(req):
    err = _require_apikey(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    merchant_account = body.get("merchantAccount", "")
    if merchant_account == None or merchant_account == "":
        return _adyen_err(422, "100", "merchantAccount is missing", "validation")

    payment_methods = [
        {
            "type": "scheme",
            "name": "Credit Card",
            "brands": ["visa", "mc", "amex", "discover"],
        },
        {
            "type": "ideal",
            "name": "iDEAL",
            "issuers": [
                {"id": "1121", "name": "Test Issuer"},
                {"id": "1154", "name": "Test Issuer 4"},
            ],
        },
        {
            "type": "paypal",
            "name": "PayPal",
        },
        {
            "type": "applepay",
            "name": "Apple Pay",
        },
        {
            "type": "googlepay",
            "name": "Google Pay",
        },
    ]

    return respond(200, {
        "paymentMethods": payment_methods,
        "storedPaymentMethods": [],
    })
