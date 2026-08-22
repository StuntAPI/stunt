# Run handler — Google Apps Script API.
#
# POST /v1/scripts/{scriptId}:run  → canonical route (function rides the body)
# POST /v1/projects/{scriptId}/scripts/{functionName}/run → legacy alias
#   Body: {function, devMode, parameters:[...]}

# on_run_script serves the canonical scripts.run route: the function name
# comes from the request body, per the real API.
def on_run_script(req):
    body = req["body"]
    if body == None:
        body = {}
    return _run(req, req["params"]["scriptId"], str(body.get("function", "")), body)

# on_run serves the legacy path-parameterized alias.
def on_run(req):
    body = req["body"]
    if body == None:
        body = {}
    return _run(req, req["params"]["scriptId"], req["params"]["functionName"], body)

def _run(req, script_id, function_name, body):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    project = _find_project(script_id)
    if project == None:
        return _g_err(404, "Project " + script_id + " not found.", "NOT_FOUND")

    dev_mode = body.get("devMode", False)
    if dev_mode == None:
        dev_mode = False
    parameters = body.get("parameters", [])
    if parameters == None:
        parameters = []

    # Simulate function execution.
    result = _simulate_function(function_name, parameters)

    return respond(200, {
        "response": {
            "result": result,
        },
        "done": True,
        "name": "operations/run-" + str(store_kv_incr("apps-script", "run_seq") + 1),
        "metadata": {
            "scriptId": script_id,
            "function": function_name,
            "devMode": dev_mode,
        },
    })

# _simulate_function returns synthetic results for known function patterns.
def _simulate_function(name, parameters):
    if name == None or name == "":
        return None

    # If function name contains "hello" or "greet", return a greeting.
    if _contains(name, "hello") or _contains(name, "greet"):
        if len(parameters) > 0:
            return "Hello, " + str(parameters[0]) + "!"
        return "Hello, World!"

    # If function name contains "add" and has 2 params, return sum.
    # JSON numbers arrive as floats (19 -> 19.0); strip the .0 before the
    # strict digit parse, or integral inputs would sum to 0.
    if _contains(name, "add") and len(parameters) >= 2:
        a = parameters[0]
        b = parameters[1]
        if type(a) == "float" and a == int(a):
            a = int(a)
        if type(b) == "float" and b == int(b):
            b = int(b)
        return _to_int(str(a)) + _to_int(str(b))

    # If function name contains "status", return a status dict.
    if _contains(name, "status"):
        return {"status": "OK", "active": True, "count": len(parameters)}

    # Default: return the parameters echoed back.
    return parameters

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0
