# Work items handlers — Azure DevOps WIT (Work Item Tracking).
#
# GET  /{org}/{project}/_apis/wit/workitems/{id} → work item resource
# POST /{org}/{project}/_apis/wit/workitems       → create work item
#
# NOTE: Azure DevOps work item IDs are integers, but the stunt collection
# store requires string IDs. We store the numeric ID in "wi_id" and the
# collection "id" as a string, then return the integer in responses.

# on_get_workitem returns a single work item by id.
def on_get_workitem(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    wi_id_str = req["params"]["id"]
    wc = store_collection("workitems")
    for wi in wc.list():
        if wi.get("id") == wi_id_str:
            return respond(200, _workitem_resource(wi))

    return respond(404, {
        "$id": "1",
        "innerException": None,
        "message": "The work item " + wi_id_str + " does not exist.",
        "typeName": "Microsoft.TeamFoundation.WorkItemTracking.Client.WorkItemDoesNotExistException",
        "typeKey": "WorkItemDoesNotExistException",
        "errorCode": 0,
        "eventId": 3200,
    })

# on_create_workitem creates a new work item.
# Azure DevOps uses a PATCH-style JSON body ([{op:"add", path, value}]).
def on_create_workitem(req):
    token, err = _require_auth(req)
    if err != None:
        return err

    _seed()

    body = req["body"]
    if body == None:
        body = {}

    wi_num = store_kv_incr("azure-devops", "wi_seq") + 1
    wi_id_str = str(wi_num)

    fields = {
        "System.AreaPath": "MyFirstProject",
        "System.TeamProject": "MyFirstProject",
        "System.IterationPath": "MyFirstProject",
        "System.WorkItemType": "Task",
        "System.State": "New",
        "System.Reason": "New",
        "System.CreatedDate": "2024-01-01T00:00:00.000Z",
        "System.CreatedBy": "Test User <test@example.com>",
        "System.ChangedDate": "2024-01-01T00:00:00.000Z",
        "System.ChangedBy": "Test User <test@example.com>",
    }

    # Process the PATCH-style body operations.
    # JSON array bodies are wrapped as {_batch: [...]} by the engine.
    ops = body
    batch = body.get("_batch", None)
    if batch != None:
        ops = batch
    if type(ops) == "list":
        for op in ops:
            if op == None:
                continue
            path = op.get("path", "")
            value = op.get("value", "")
            if path[:1] == "/":
                path = path[1:]
            # Map fields/System.Title -> System.Title
            if path[:7] == "fields/":
                field_name = path[7:]
                fields[field_name] = value

    new_wi = {
        "id": wi_id_str,
        "wi_id": wi_num,
        "rev": 1,
        "fields": fields,
        "url": "https://dev.azure.com/mock-org/MyFirstProject/_apis/wit/workItems/" + wi_id_str,
    }

    wc = store_collection("workitems")
    wc.insert(new_wi)

    return respond(200, _workitem_resource(new_wi))

# _workitem_resource builds the API response shape for a work item.
# Returns the integer id (wi_id) in the response, matching real API.
def _workitem_resource(wi):
    return {
        "id": wi.get("wi_id", _to_int(wi.get("id", "0"))),
        "rev": wi.get("rev", 1),
        "fields": wi.get("fields", {}),
        "_links": {
            "self": {
                "href": wi.get("url", ""),
            },
            "html": {
                "href": "https://dev.azure.com/mock-org/MyFirstProject/_workitems/edit/" + str(wi.get("wi_id", wi.get("id", "0"))),
            },
        },
        "url": wi.get("url", ""),
    }
