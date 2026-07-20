# Notification handler — simulates the merchant-side notification receiver.
#
# Adyen sends HMAC-signed notifications to the merchant's webhook URL.
# The notification is a batch of NotificationRequestItems, each with an
# eventCode (e.g. AUTHORISATION, CAPTURE, REFUND), and signed via HMAC.
#
# POST /v68/notifications/test
#
# This endpoint accepts the notification (returns "[accepted]") and
# documents the HMAC verification scheme. See README.md for the exact
# signature computation.
#
# Notification shape (Adyen standard):
# {
#   "live": "false",
#   "notificationItems": [
#     {
#       "NotificationRequestItem": {
#         "additionalData": {
#           "hmacSignature": "coqCmt7IZ7Mn_no..."  // base64 HMAC-SHA256
#         },
#         "amount": { "value": 1000, "currency": "USD" },
#         "eventCode": "AUTHORISATION",
#         "eventDate": "2024-01-01T00:00:00+01:00",
#         "merchantAccountCode": "TestMerchant",
#         "merchantReference": "ref-001",
#         "pspReference": "8814000000000001",
#         "reason": "",
#         "success": "true"
#       }
#     }
#   ]
# }
#
# HMAC signature scheme (documented — not computed in Starlark):
#
# 1. Build the "data to sign" by concatenating these field values (in this
#    exact order) from the NotificationRequestItem, escaping each value:
#      - pspReference
#      - originalReference (empty for non-modifications)
#      - merchantAccountCode
#      - merchantReference
#      - amount.value (as integer string, e.g. "1000")
#      - amount.currency (e.g. "USD")
#      - eventCode
#      - success ("true" or "false")
#    Escaping: replace "\" with "\\", replace ":" with "\:", (Adyen also
#    escapes these in the HMAC util).
# 2. Separate each value with a colon ":".
# 3. Base64-encode the resulting string → this is the "data to sign".
# 4. HMAC-SHA256(hmac_key, data_to_sign_base64) → raw bytes.
# 5. Base64-encode the HMAC bytes → hmacSignature.
#
# In Go:
#   import (
#     "crypto/hmac"
#     "crypto/sha256"
#     "encoding/base64"
#   )
#
#   func computeHmac(key string, data []byte) string {
#     h := hmac.New(sha256.New, []byte(key))
#     h.Write(data)
#     return base64.StdEncoding.EncodeToString(h.Sum(nil))
#   }
#
#   // dataToSign is the base64 of the colon-joined escaped fields
#   sig := computeHmac(hmacKey, []byte(base64.StdEncoding.EncodeToString([]byte(dataToSign))))

def on_notification(req):
    err = _require_apikey(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    # Accept the notification. In real Adyen flow, the merchant would verify
    # the HMAC signature here, then process the notification.
    notification_items = body.get("notificationItems", [])

    # Process each notification item (store for stateful tracking).
    mc = store_collection("modifications")
    for item in notification_items:
        nri = item.get("NotificationRequestItem", {})
        if nri != None:
            psp_ref = nri.get("pspReference", "")
            event_code = nri.get("eventCode", "")
            success = nri.get("success", "false")

            # Store the notification for test verification.
            mc.insert({
                "id": psp_ref + "-" + event_code,
                "type": "notification",
                "eventCode": event_code,
                "pspReference": psp_ref,
                "success": success,
            })

    # Adyen expects the literal string "[accepted]" as response.
    return respond(202, "[accepted]")
