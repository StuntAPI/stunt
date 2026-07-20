# Model handlers — Anaplan API.
#
# GET /2/0/workspaces/{wid}/models          → {meta, items:[{id, name, active}]}
# GET /2/0/workspaces/{wid}/models/{mid}    → single model
# GET /2/0/workspaces/{wid}/models/{mid}/modules → {meta, items:[...]}

# Seed models per workspace.
_MODELS = {
    "8a819c8645a0aa8e0005c715c7ad49b9": [
        {"id": "A101", "name": "Demand Planning Model", "active": True, "modelType": "Production"},
        {"id": "A102", "name": "Inventory Model", "active": True, "modelType": "Production"},
    ],
    "8a819c8645b1bb9f0006c825d8be50c0": [
        {"id": "B201", "name": "Revenue Forecast", "active": True, "modelType": "Production"},
    ],
}

def on_list_models(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    ws_id = req["params"]["workspaceId"]
    models = _MODELS.get(ws_id, [])
    if models == None:
        models = []

    return respond(200, {
        "meta": {
            "paging": {
                "currentPageSize": len(models),
                "offset": 0,
                "totalSize": len(models),
            },
        },
        "items": models,
    })

def on_get_model(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    ws_id = req["params"]["workspaceId"]
    model_id = req["params"]["modelId"]
    models = _MODELS.get(ws_id, [])
    if models == None:
        models = []

    for model in models:
        if model.get("id") == model_id:
            return respond(200, model)

    return respond(404, {
        "status": "FAILURE",
        "statusMessage": "Model " + model_id + " not found",
    })

def on_list_modules(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    model_id = req["params"]["modelId"]

    modules = [
        {"id": "mod001", "name": "Revenue", "activeState": True, "dataSource": False},
        {"id": "mod002", "name": "Expenses", "activeState": True, "dataSource": False},
        {"id": "mod003", "name": "Headcount", "activeState": True, "dataSource": False},
    ]

    return respond(200, {
        "meta": {
            "paging": {
                "currentPageSize": len(modules),
                "offset": 0,
                "totalSize": len(modules),
            },
        },
        "items": modules,
    })
