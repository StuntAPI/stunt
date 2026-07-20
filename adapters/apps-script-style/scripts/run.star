# Run handler — Google Apps Script API.
#
# POST /v1/projects/{scriptId}/scripts/{functionName}/run → run a function
#   Body: {function, devMode, parameters:[...]}

# Synthetically "executes" known functions and returns results.
def on_run(req):
    err = _require_bearer(req)
    if err != None:
        return err

    _seed()

    script_id = req["params"]["scriptId"]
    function_name = req["params"]["functionName"]

    project = _find_project(script_id)
    if project == None:
        return _g_err(404, "Project " + script_id + " not found.", "NOT_FOUND")

    body = req["body"]
    if body == None:
        body = {}

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
    if _contains(name, "add") and len(parameters) >= 2:
        return _to_int(str(parameters[0])) + _to_int(str(parameters[1]))

    # If function name contains "status", return a status dict.
    if _contains(name, "status"):
        return {"status": "OK", "active": True, "count": len(parameters)}

    # Default: return the parameters echoed back.
    return parameters

# _contains reports whether substr appears within s.
def _contains(s, substr):
    return s.find(substr) >= 0

# _to_int parses a decimal string to int.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n
