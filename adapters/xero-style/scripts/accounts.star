# Accounts handler — chart of accounts.
#
# Requires Bearer + xero-tenant-id.
# GET /api.xro/2.0/Accounts → { Id, Status, Accounts: [...] }

# on_list_accounts returns the chart of accounts.
def on_list_accounts(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    _ensure_accounts()

    c = store_collection("accounts")
    docs = c.list()

    accounts = []
    for doc in docs:
        accounts.append({
            "AccountID": doc.get("AccountID", ""),
            "Code": doc.get("Code", ""),
            "Name": doc.get("Name", ""),
            "Type": doc.get("Type", ""),
            "Status": doc.get("Status", "ACTIVE"),
            "Class": doc.get("Class", ""),
        })

    return _envelope("Accounts", accounts)
