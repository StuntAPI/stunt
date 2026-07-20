# Bank Transactions handler.
#
# Requires Bearer + xero-tenant-id.
# GET /api.xro/2.0/BankTransactions → { Id, Status, BankTransactions: [...] }

# on_list_bank_transactions returns synthetic bank transactions.
def on_list_bank_transactions(req):
    err = _require_auth(req)
    if err != None:
        return err
    err = _require_tenant(req)
    if err != None:
        return err

    return _envelope("BankTransactions", [
        {
            "BankTransactionID": _guid(301),
            "Type": "RECEIVE",
            "Reference": "Deposit",
            "Date": "2024-06-15T00:00:00",
            "Status": "AUTHORISED",
            "LineItems": [{"Description": "Bank deposit", "LineAmount": "500.00"}],
        },
        {
            "BankTransactionID": _guid(302),
            "Type": "SPEND",
            "Reference": "Office supplies",
            "Date": "2024-06-10T00:00:00",
            "Status": "AUTHORISED",
            "LineItems": [{"Description": "Office supplies", "LineAmount": "75.50"}],
        },
    ])
