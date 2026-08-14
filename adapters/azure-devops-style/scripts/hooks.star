# Service hooks handlers — Azure DevOps generic webhook subscriptions.
#
# POST /{org}/_apis/hooks/subscriptions -> subscription object (register)
#
# The real service-hooks "webHooks" consumer (consumerId "webHooks",
# consumerActionId "httpRequest") POSTs the event envelope to the URL in
# consumerInputs.url. Azure DevOps signs NOTHING: deliveries are
# UNSIGNED BY DESIGN — authentication (basic auth, bearer token, or custom
# headers) is configured on the subscription itself. stunt therefore emits
# events with the real service-hook payload envelope but no signature headers.
# See scripts/lib.star (_emit_service_hook) and the adapter README.

# on_create_subscription registers a generic webhook service-hook
# subscription. Real request shape:
#   {
#     "consumerId": "webHooks",
#     "consumerActionId": "httpRequest",
#     "eventType": "workitem.created",
#     "consumerInputs": {"url": "https://...", "httpMethod": "POST", ...},
#     "publisherInputs": {"projectId": "..."},
#     "resourceVersion": "1.0"
#   }
def on_create_subscription(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    consumer_inputs = body.get("consumerInputs", {})
    if consumer_inputs == None:
        consumer_inputs = {}
    url = consumer_inputs.get("url", "")
    if url == None:
        url = ""

    event_type = body.get("eventType", "")
    if event_type == None:
        event_type = ""

    sub_id = _next_subscription_id()

    sub = {
        "id": sub_id,
        "consumerId": body.get("consumerId", "webHooks"),
        "consumerActionId": body.get("consumerActionId", "httpRequest"),
        "eventType": event_type,
        "url": url,
        "publisherInputs": body.get("publisherInputs", {}),
        "resourceVersion": body.get("resourceVersion", "1.0"),
        "status": "enabled",
    }

    sc = store_collection("subscriptions")
    sc.insert(sub)

    # Register the delivery URL with the events emitter.
    if url != "":
        events_register(url)

    return respond(200, _subscription_resource(sub))

# _subscription_resource builds the API response shape for a subscription.
def _subscription_resource(sub):
    return {
        "id": sub.get("id", ""),
        "consumerId": sub.get("consumerId", "webHooks"),
        "consumerActionId": sub.get("consumerActionId", "httpRequest"),
        "eventType": sub.get("eventType", ""),
        "consumerInputs": {"url": sub.get("url", "")},
        "publisherInputs": sub.get("publisherInputs", {}),
        "resourceVersion": sub.get("resourceVersion", "1.0"),
        "status": sub.get("status", "enabled"),
        "_links": {
            "self": {
                "href": "https://dev.azure.com/mock-org/_apis/hooks/subscriptions/" + sub.get("id", ""),
            },
        },
    }
