# Account handlers — list accounts, get balances, get transactions.
#
# These endpoints require a valid consent (after SCA finalisation).
#
# GET /v1/accounts                         → { accounts: [{ resourceId, iban, currency, name }] }
# GET /v1/accounts/{resourceId}/balances   → { account:{ iban }, balances:[{ balanceAmount, balanceType, ... }] }
# GET /v1/accounts/{resourceId}/transactions → { transactions:{ booked:[...], pending:[...] } }

# on_list_accounts returns the PSU's accounts (requires valid consent).
# The real API honors the withBalance query param (include balance data in
# each account object when true).
def on_list_accounts(req):
    err = _require_consent(req)
    if err != None:
        return err

    ac = store_collection("accounts")
    all_accounts = ac.list()
    with_balance = _get_query(req).get("withBalance", "")
    if with_balance == None:
        with_balance = ""
    with_balance = with_balance.lower() == "true"

    result = []
    for a in all_accounts:
        entry = {
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
        }
        if with_balance:
            entry["balances"] = [
                {
                    "balanceAmount": {
                        "currency": a.get("currency", "EUR"),
                        "amount": a.get("bookedBalance", "5000") + ".00",
                    },
                    "balanceType": "interimBooked",
                    "lastChangeDateTime": "2024-01-01T00:00:00Z",
                    "referenceDate": "2024-01-01",
                },
            ]
        result.append(entry)

    # Apply Berlin Group NextGenPSD2 pagination (page/size) to the account list.
    page, next_cursor = _list_page(req, result)

    self_href = "https://api.stunt.test/v1/accounts"
    size_hint = str(_to_int(_get_query(req).get("size", "")))

    links = {
        "self": {"href": self_href},
    }
    links.update(_page_links(self_href, next_cursor, size_hint))

    return respond(200, {
        "accounts": page,
        "balances": [],
        "_links": links,
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

    # Real NextGenPSD2 transaction-report params, applied after account
    # scoping: bookingStatus (booked/pending/both, default both) selects
    # which lists are populated; dateFrom/dateTo bound the bookingDate.
    booking_status = _get_query(req).get("bookingStatus", "")
    if booking_status == None or booking_status == "":
        booking_status = "both"
    if booking_status == "booked":
        pending = []
    elif booking_status == "pending":
        booked = []

    booked = _apply_tx_filters(req, booked)
    pending = _apply_tx_filters(req, pending)

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

# --- helpers ---

# _apply_tx_filters maps the NextGenPSD2 dateFrom/dateTo transaction-report
# query params to query_select clauses. bookingDate is stored as an ISO date
# string, so lexicographic >=/<= comparisons match the real date semantics.
def _apply_tx_filters(req, txs):
    f = []
    date_from = _get_query(req).get("dateFrom", "")
    if date_from != None and date_from != "":
        f.append(["bookingDate", ">=", date_from])
    date_to = _get_query(req).get("dateTo", "")
    if date_to != None and date_to != "":
        f.append(["bookingDate", "<=", date_to])
    if len(f) == 0:
        return txs
    return query_select(txs, f)
