# Flows handlers — Microsoft Power Automate flows.
#
# GET  /v2/environments/{env}/flows → OData {value:[...]}
# POST /v2/environments/{env}/flows → create a flow

def on_list_flows(req):
    err = _require_bearer(req)
    if err != None:
        return err

    env = req["params"]["env"]

    fc = store_collection("flows")
    items = []
    for flow in fc.list():
        if flow.get("environment") == env:
            items.append(_flow_resource(flow))

    # Add a seeded flow if no flows exist.
    if len(items) == 0:
        items.append({
            "name": "seeded-flow-001",
            "id": "/providers/Microsoft.Flow/flows/seeded-flow-001",
            "type": "Microsoft.Flow/flows",
            "properties": {
                "displayName": "Welcome Email Flow",
                "state": "Enabled",
                "environment": env,
            },
        })

    return respond(200, {"value": items})

def on_create_flow(req):
    err = _require_bearer(req)
    if err != None:
        return err

    env = req["params"]["env"]
    body = req["body"]
    if body == None:
        body = {}

    display_name = body.get("properties", {}).get("displayName", "New Flow")
    if display_name == None:
        display_name = "New Flow"

    seq = store_kv_incr("powerplatform", "flow_seq") + 1
    flow_id = "flow-" + str(seq)

    flow = {
        "id": flow_id,
        "name": flow_id,
        "environment": env,
        "properties": {
            "displayName": display_name,
            "state": "Enabled",
        },
    }

    fc = store_collection("flows")
    fc.insert(flow)

    return respond(201, _flow_resource(flow))

def _flow_resource(flow):
    return {
        "name": flow.get("name", ""),
        "id": "/providers/Microsoft.Flow/flows/" + flow.get("id", flow.get("name", "")),
        "type": "Microsoft.Flow/flows",
        "properties": flow.get("properties", {}),
    }
