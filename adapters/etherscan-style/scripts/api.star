# Etherscan-style API handler — dispatches all module/action combinations.
#
# GET /api?apikey=...&module=...&action=...&...
#
# Reproduces the EXACT Etherscan response envelope:
#   { "status": "1"|"0", "message": "OK"|"NOTOK", "result": <data> }
#
# The result is often a STRING (numbers as strings), sometimes an array.

# Shared helpers (_ok, _err, _require_apikey, _seed, etc.) are preloaded
# from scripts/lib.star.

# on_api is the single entry point for all Etherscan API calls.
def on_api(req):
    auth_err = _require_apikey(req)
    if auth_err != None:
        return auth_err

    _seed()

    module = req["query"].get("module", "")
    if module == None:
        module = ""
    action = req["query"].get("action", "")
    if action == None:
        action = ""

    # Dispatch on module + action.
    if module == "account":
        return _handle_account(req, action)
    elif module == "contract":
        return _handle_contract(req, action)
    elif module == "token":
        return _handle_token(req, action)
    elif module == "stats":
        return _handle_stats(req, action)
    elif module == "transaction":
        return _handle_transaction(req, action)
    elif module == "block":
        return _handle_block(req, action)
    elif module == "logs":
        return _handle_logs(req, action)
    elif module == "proxy":
        return _handle_proxy(req, action)
    else:
        return respond(200, _err("Invalid module"))

# --- module: account ---

def _handle_account(req, action):
    address = req["query"].get("address", "")

    if action == "balance":
        return _account_balance(req, address)
    elif action == "balancemulti":
        return _account_balance_multi(req)
    elif action == "txlist":
        return _account_txlist(req, address)
    elif action == "txlistinternal":
        return respond(200, _ok([]))
    elif action == "tokentx":
        return respond(200, _ok([]))
    elif action == "tokenbalance":
        return respond(200, _ok("0"))
    elif action == "tokennfttx":
        return respond(200, _ok([]))
    else:
        return respond(200, _err("Invalid account action"))

def _account_balance(req, address):
    if address == None or address == "":
        return respond(200, _err("Missing address"))

    ac = store_collection("accounts")
    for acct in ac.list():
        if acct.get("address", "").lower() == address.lower():
            return respond(200, _ok(acct.get("balance", "0")))

    # Default balance for unknown addresses.
    return respond(200, _ok("0"))

def _account_balance_multi(req):
    addresses_str = req["query"].get("address", "")
    if addresses_str == None:
        addresses_str = ""
    addresses = _split_commas(addresses_str)

    ac = store_collection("accounts")
    all_accounts = ac.list()

    result = []
    for addr in addresses:
        balance = "0"
        for acct in all_accounts:
            if acct.get("address", "").lower() == addr.lower():
                balance = acct.get("balance", "0")
                break
        result.append({"account": addr, "balance": balance})

    return respond(200, _ok(result))

def _account_txlist(req, address):
    if address == None or address == "":
        return respond(200, _err("Missing address"))

    tc = store_collection("transactions")
    result = []
    for tx in tc.list():
        if tx.get("from", "").lower() == address.lower() or tx.get("to", "").lower() == address.lower():
            result.append({
                "hash": tx.get("hash", ""),
                "from": tx.get("from", ""),
                "to": tx.get("to", ""),
                "value": tx.get("value", "0"),
                "timeStamp": tx.get("timeStamp", "0"),
                "gasUsed": tx.get("gasUsed", "0"),
                "gasPrice": tx.get("gasPrice", "0"),
                "isError": tx.get("isError", "0"),
                "txreceipt_status": tx.get("txreceipt_status", "0"),
                "nonce": tx.get("nonce", "0"),
                "blockNumber": tx.get("blockNumber", "0"),
                "transactionIndex": tx.get("transactionIndex", "0"),
                "input": tx.get("input", "0x"),
                "confirmations": tx.get("confirmations", "0"),
            })

    return respond(200, _ok(result))

# --- module: contract ---

def _handle_contract(req, action):
    address = req["query"].get("address", "")

    if action == "getabi":
        return _contract_getabi(req, address)
    elif action == "getsourcecode":
        return _contract_getsourcecode(req, address)
    else:
        return respond(200, _err("Invalid contract action"))

def _contract_getabi(req, address):
    if address == None or address == "":
        return respond(200, _err("Missing address"))

    cc = store_collection("contracts")
    for c in cc.list():
        if c.get("address", "").lower() == address.lower():
            return respond(200, _ok(c.get("ABI", "")))

    return respond(200, _err("Contract source code not verified"))

def _contract_getsourcecode(req, address):
    if address == None or address == "":
        return respond(200, _err("Missing address"))

    cc = store_collection("contracts")
    for c in cc.list():
        if c.get("address", "").lower() == address.lower():
            return respond(200, _ok([{
                "SourceCode": c.get("SourceCode", ""),
                "ABI": c.get("ABI", ""),
                "ContractName": c.get("ContractName", ""),
                "CompilerVersion": c.get("CompilerVersion", ""),
                "OptimizationUsed": c.get("OptimizationUsed", ""),
                "Runs": c.get("Runs", ""),
                "ConstructorArguments": c.get("ConstructorArguments", ""),
                "EVMVersion": c.get("EVMVersion", ""),
                "Library": c.get("Library", ""),
                "LicenseType": c.get("LicenseType", ""),
                "Proxy": c.get("Proxy", ""),
                "Implementation": c.get("Implementation", ""),
                "SwarmSource": c.get("SwarmSource", ""),
            }]))

    # Unverified contracts return an empty source array.
    return respond(200, _ok([{
        "SourceCode": "",
        "ABI": "Contract source code not verified",
        "ContractName": "",
        "CompilerVersion": "",
        "OptimizationUsed": "",
        "Runs": "",
        "ConstructorArguments": "",
        "EVMVersion": "",
        "Library": "",
        "LicenseType": "",
        "Proxy": "",
        "Implementation": "",
        "SwarmSource": "",
    }]))

# --- module: token ---

def _handle_token(req, action):
    if action == "tokenholderlist":
        return _token_holders(req)
    elif action == "tokenbalance":
        return respond(200, _ok("0"))
    elif action == "tokensupply":
        return respond(200, _ok("750000000000000000000"))
    elif action == "tokeninfo":
        return respond(200, _ok([]))
    else:
        return respond(200, _err("Invalid token action"))

def _token_holders(req):
    contract = req["query"].get("contractaddress", "")
    hc = store_collection("token_holders")
    result = []
    for h in hc.list():
        result.append({
            "TokenHolderAddress": h.get("TokenHolderAddress", ""),
            "TokenHolderQuantity": h.get("TokenHolderQuantity", "0"),
        })
    return respond(200, _ok(result))

# --- module: stats ---

def _handle_stats(req, action):
    if action == "ethsupply":
        return respond(200, _ok("120000000000000000000000000"))
    elif action == "ethsupply2":
        return respond(200, _ok({"EthSupply": "120000000000000000000000000"}))
    elif action == "ethprice":
        return respond(200, _ok({
            "ethbtc": "15.0",
            "ethbtc_timestamp": "1700000000",
            "ethusd": "2500.00",
            "ethusd_timestamp": "1700000000",
        }))
    elif action == "blocksize":
        return respond(200, _ok("50000"))
    elif action == "chaindatasize":
        return respond(200, _ok("1000000000000"))
    elif action == "dailyavgblocksize":
        return respond(200, _ok("50000"))
    elif action == "nodecount":
        return respond(200, _ok("10000"))
    elif action == "totalgasprice":
        return respond(200, _ok("1000000000"))
    else:
        return respond(200, _err("Invalid stats action"))

# --- module: transaction ---

def _handle_transaction(req, action):
    txhash = req["query"].get("txhash", "")

    if action == "getstatus":
        return respond(200, _ok("1"))
    elif action == "gettxreceiptstatus":
        return respond(200, _ok("1"))
    else:
        return respond(200, _err("Invalid transaction action"))

# --- module: block ---

def _handle_block(req, action):
    if action == "getblockreward":
        return respond(200, _ok({
            "blockNumber": "1",
            "blockMiner": "0x0000000000000000000000000000000000000000",
            "blockReward": "2000000000000000000",
        }))
    elif action == "getblocknobytime":
        return respond(200, _ok("1"))
    else:
        return respond(200, _err("Invalid block action"))

# --- module: logs ---

def _handle_logs(req, action):
    return respond(200, _ok([]))

# --- module: proxy (passthrough to JSON-RPC, simplified) ---

def _handle_proxy(req, action):
    if action == "eth_blockNumber":
        return respond(200, _ok("0x0"))
    elif action == "eth_getBalance":
        return respond(200, _ok("0x0"))
    else:
        return respond(200, _ok("0x0"))

# --- helpers ---

# _split_commas splits a comma-separated string into a list.
def _split_commas(s):
    if s == None or s == "":
        return []
    result = []
    current = ""
    for i in range(len(s)):
        if s[i] == ",":
            if current != "":
                result.append(current)
            current = ""
        else:
            current = current + s[i]
    if current != "":
        result.append(current)
    return result
