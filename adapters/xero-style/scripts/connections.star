# Connections handler — the Xero tenant list.
#
# Only requires Bearer auth (no xero-tenant-id — this IS the tenant list).
#
# GET /connections → { connections: [{ id, tenantId, tenantType, tenantName }] }

# on_list_connections returns the Xero connections (tenant list).
def on_list_connections(req):
    err = _require_auth(req)
    if err != None:
        return err

    return respond(200, {
        "connections": [
            {
                "id": "00000000-0000-0000-0000-000000000001",
                "tenantId": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
                "tenantType": "ORGANISATION",
                "tenantName": "Demo Company (US)",
                "createdDateUtc": "2024-01-01T00:00:00.000Z",
            },
            {
                "id": "00000000-0000-0000-0000-000000000002",
                "tenantId": "b2c3d4e5-f678-901a-bcde-f12345678901",
                "tenantType": "ORGANISATION",
                "tenantName": "Demo Company (UK)",
                "createdDateUtc": "2024-01-01T00:00:00.000Z",
            },
        ],
    })
