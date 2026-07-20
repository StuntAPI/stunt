# IAM Roles query handler — queryGrantableRoles.
#
# POST /v1/projects/{project}/roles:queryGrantableRoles (Bearer)
#
# This simulates the "who can do what" query that is central to IAM pain.
# Given a full resource name, it returns the roles that can be granted on it.

# Shared helpers (_bearer, _require_bearer) are preloaded from scripts/lib.star.

# Predefined roles for common resource types.
_ROLES_BY_TYPE = {
    "projects": [
        {"name": "roles/owner", "title": "Owner", "description": "Full access to all resources."},
        {"name": "roles/editor", "title": "Editor", "description": "Edit access to all resources."},
        {"name": "roles/viewer", "title": "Viewer", "description": "Read access to all resources."},
    ],
    "storage": [
        {"name": "roles/storage.objectAdmin", "title": "Storage Object Admin", "description": "Full control of objects."},
        {"name": "roles/storage.objectViewer", "title": "Storage Object Viewer", "description": "View objects."},
    ],
    "pubsub": [
        {"name": "roles/pubsub.publisher", "title": "Pub/Sub Publisher", "description": "Publish messages."},
        {"name": "roles/pubsub.subscriber", "title": "Pub/Sub Subscriber", "description": "Consume messages."},
    ],
}

def on_query_grantable_roles(req):
    _, err = _require_bearer(req)
    if err != None:
        return err

    body = req["body"]
    if body == None:
        body = {}

    full_resource_name = body.get("fullResourceName", "")
    if full_resource_name == "":
        return respond(400, {
            "error": {
                "code": 400,
                "message": "fullResourceName is required.",
                "status": "INVALID_ARGUMENT",
            },
        })

    # Determine the resource type from the resource name prefix.
    resource_type = "projects"  # default
    if _contains(full_resource_name, "storage"):
        resource_type = "storage"
    elif _contains(full_resource_name, "pubsub"):
        resource_type = "pubsub"

    roles = _ROLES_BY_TYPE.get(resource_type, _ROLES_BY_TYPE["projects"])

    # Format roles in the API response shape.
    formatted = []
    for r in roles:
        formatted.append({
            "name": r["name"],
            "title": r["title"],
            "description": r["description"],
            "stage": "GA",
            "deleted": False,
        })

    return respond(200, {"roles": formatted})
