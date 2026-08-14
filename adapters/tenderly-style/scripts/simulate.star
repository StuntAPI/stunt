# Simulation handlers — Tenderly Simulation API.
#
# POST .../simulate → {transaction:{status:true, gas_used, ...}, ...}
# POST .../simulate-bundle → {results:[{transaction:{...}}], ...}

# Shared helpers (_bearer, _require_auth, _err, _build_simulation_result)
# are preloaded.

def on_simulate(req):
    if not _require_auth(req):
        return respond(401, _err("unauthorized", "Missing or invalid API key"))

    body = req["body"]
    if body == None:
        body = {}

    account = req["params"]["account"]
    project = req["params"]["project"]

    result = _build_simulation_result(body, account, project)

    # Store the simulation (stateful).
    sc = store_collection("simulations")
    sc.insert({
        "id": result["simulationId"],
        "account": account,
        "project": project,
        "result": result,
    })

    return respond(200, result)

def on_simulate_bundle(req):
    if not _require_auth(req):
        return respond(401, _err("unauthorized", "Missing or invalid API key"))

    body = req["body"]
    if body == None:
        body = {}

    account = req["params"]["account"]
    project = req["params"]["project"]

    # A bundle is a list of simulations.
    simulations = body.get("simulations", [])
    if len(simulations) == 0:
        return respond(400, _err("bad_request", "simulations array must not be empty"))

    results = []
    for sim_body in simulations:
        result = _build_simulation_result(sim_body, account, project)
        sc = store_collection("simulations")
        sc.insert({
            "id": result["simulationId"],
            "account": account,
            "project": project,
            "result": result,
        })
        results.append(result)

    return respond(200, {
        "simulation_results": results,
        "bundle_id": "bundle_" + results[0]["simulationId"],
    })

# GET .../simulations → list stored simulations for this account.
def on_list_simulations(req):
    if not _require_auth(req):
        return respond(401, _err("unauthorized", "Missing or invalid API key"))

    account = req["params"]["account"]
    sc = store_collection("simulations")
    out = []
    for doc in sc.list():
        if doc.get("account") == account:
            out.append(doc.get("result", doc))
    return respond(200, {"simulations": out})

# GET .../simulations/{id} → retrieve a stored simulation by id.
def on_retrieve_simulation(req):
    if not _require_auth(req):
        return respond(401, _err("unauthorized", "Missing or invalid API key"))

    id = req["params"]["id"]
    sc = store_collection("simulations")
    doc = sc.get(id)
    if doc == None:
        return respond(404, _err("not_found", "simulation " + id + " not found"))
    return respond(200, doc.get("result", doc))
