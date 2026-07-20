# Account handlers — list accounts, get balances, get transactions.
#
# These endpoints require a valid consent (after SCA finalisation).
#
# GET /v1/accounts                         → { accounts: [{ resourceId, iban, currency, name }] }
# GET /v1/accounts/{resourceId}/balances   → { account:{ iban }, balances:[{ balanceAmount, balanceType, ... }] }
# GET /v1/accounts/{resourceId}/transactions → { transactions:{ booked:[...], pending:[...] } }

# on_list_accounts returns the PSU's accounts (requires valid consent).
def on_list_accounts(req):
    err = _require_consent(req)
    if err != None:
        return err

    ac = store_collection("accounts")
    all_accounts = ac.list()

    result = []
    for a in all_accounts:
        result.append({
            "resourceId": a["id"],
            "iban": a.get("iban", ""),
            "bban": "",
            "pan": "",
            "maskedPan": "",
            "msisdn": "",
            "currency": a.get("currency", "EUR"),
            "name": a.get("name", ""),
            "product": a.get("product", "Current Account"),
            "cashAccountType": "CASH",
            "status": "enabled",
            "bic": a.get("bic", "STNTDE01"),
            "linkedAccounts": "",
            "usage": "PRIV",
            "details": "",
            "_links": {
                "balances": {"href": "https://api.stunt.test/v1/accounts/" + a["id"] + "/balances"},
                "transactions": {"href": "https://api.stunt.test/v1/accounts/" + a["id"] + "/transactions"},
            },
        })

    return respond(200, {
        "accounts": result,
        "balances": [],
        "_links": {
            "self": {"href": "https://api.stunt.test/v1/accounts"},
        },
    })

# on_get_balances returns the balances for a specific account.
def on_get_balances(req):
    err = _require_consent(req)
    if err != None:
        return err

    resource_id = req["params"]["resourceId"]
    ac = store_collection("accounts")
    account = ac.get(resource_id)
    if account == None:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Account not found")

    iban = account.get("iban", "")
    currency = account.get("currency", "EUR")

    balances = [
        {
            "balanceAmount": {
                "currency": currency,
                "amount": account.get("bookedBalance", "5000") + ".00",
            },
            "balanceType": "interimBooked",
            "lastChangeDateTime": "2024-01-01T00:00:00Z",
            "referenceDate": "2024-01-01",
            "lastCommittedTransaction": "tx-001",
        },
        {
            "balanceAmount": {
                "currency": currency,
                "amount": account.get("availableBalance", "4800") + ".00",
            },
            "balanceType": "forwardAvailable",
            "lastChangeDateTime": "2024-01-01T00:00:00Z",
        },
    ]

    return respond(200, {
        "account": {
            "iban": iban,
            "currency": currency,
            "resourceId": resource_id,
        },
        "balances": balances,
    })

# on_get_transactions returns the transactions for a specific account.
def on_get_transactions(req):
    err = _require_consent(req)
    if err != None:
        return err

    resource_id = req["params"]["resourceId"]
    ac = store_collection("accounts")
    account = ac.get(resource_id)
    if account == None:
        return _psd2_err(404, "ERROR", "RESOURCE_UNKNOWN", "Account not found")

    iban = account.get("iban", "")
    currency = account.get("currency", "EUR")

    # Get transactions from the transactions collection.
    tc = store_collection("transactions")
    all_txs = tc.list()

    booked = []
    pending = []
    for t in all_txs:
        if t.get("accountId", "") != resource_id:
            continue

        tx = {
            "transactionId": t["id"],
            "bookingDate": t.get("bookingDate", "2024-01-01"),
            "valueDate": t.get("valueDate", "2024-01-01"),
            "transactionAmount": {
                "currency": currency,
                "amount": t.get("amount", "0.00"),
            },
            "remittanceInformationUnstructured": t.get("description", ""),
            "transactionDetails": t.get("description", ""),
            "debtorName": t.get("debtorName", ""),
            "creditorName": t.get("creditorName", ""),
            "mandateId": "",
            "transactionType": t.get("type", "OTHER"),
            "proprietaryBankTransactionCode": "",
        }

        if t.get("status", "booked") == "pending":
            pending.append(tx)
        else:
            booked.append(tx)

    return respond(200, {
        "account": {
            "iban": iban,
            "currency": currency,
            "resourceId": resource_id,
        },
        "transactions": {
            "booked": booked,
            "pending": pending,
            "_links": {
                "account": {"href": "https://api.stunt.test/v1/accounts/" + resource_id},
            },
        },
    })
