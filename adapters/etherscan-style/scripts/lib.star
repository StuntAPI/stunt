# Shared library for etherscan-style adapter scripts.
#
# This file is preloaded by stunt before each handler script. Its top-level
# definitions are available to all handlers as predeclared builtins.

# --- Etherscan response envelope helpers ---

# _ok wraps a successful result in the Etherscan envelope.
# result can be a string, list, or dict.
def _ok(result):
    return {"status": "1", "message": "OK", "result": result}

# _err wraps an error in the Etherscan envelope.
def _err(message):
    return {"status": "0", "message": message, "result": ""}

# --- auth ---

# _require_apikey checks for the apikey query parameter. Returns None if
# present, or an error-response dict if missing.
def _require_apikey(req):
    apikey = req["query"].get("apikey", "")
    if apikey == None or apikey == "":
        return respond(200, {"status": "0", "message": "Missing API key", "result": []})
    return None

# --- deterministic hashing (same as eth-jsonrpc for cross-adapter fidelity) ---

# _deterministic_hash produces a 64-char hex string ("0x" + 64 hex chars)
# from an input string. NOT real keccak256 — a deterministic pseudo-hash.
def _deterministic_hash(input_str):
    lanes = [
        0x811c9dc5,
        0x1000193,
        0xdeadbeef,
        0xcafebabe,
        0x12345678,
        0x9e3779b9,
        0xabcdef01,
        0x0badf00d,
    ]
    for i in range(len(input_str)):
        ch = ord(input_str[i])
        for li in range(len(lanes)):
            lanes[li] = (lanes[li] ^ ch)
            lanes[li] = (lanes[li] * 0x01000193) & 0xffffffff
            lanes[li] = ((lanes[li] << 7) | (lanes[li] >> 25)) & 0xffffffff
    hex_str = ""
    for li in range(len(lanes)):
        hex_str = hex_str + _hex32(lanes[li])
    return "0x" + hex_str

# _hex32 converts a 32-bit int to an 8-char hex string (zero-padded).
def _hex32(n):
    digits = "0123456789abcdef"
    out_chars = []
    for _ in range(8):
        out_chars.append(digits[n % 16])
        n = n // 16
    result = ""
    for i in range(len(out_chars) - 1, -1, -1):
        result = result + out_chars[i]
    return result

# _short_hash returns an 0x-prefixed 64-char hex hash for a string seed.
# Alias for readability.
def _short_hash(seed):
    return _deterministic_hash(seed)

# --- seeding ---

# _seed initializes the explorer with synthetic accounts, transactions,
# and contracts on first access.
def _seed():
    if store_kv_get("etherscan", "seeded") == "yes":
        return
    store_kv_set("etherscan", "seeded", "yes")

    # Seed well-known synthetic addresses with balances (in wei as string).
    ac = store_collection("accounts")
    ac.insert({
        "address": "0x0000000000000000000000000000000000000000",
        "balance": "1000000000000000000000",
    })
    ac.insert({
        "address": "0x0000000000000000000000000000000000000001",
        "balance": "500000000000000000000",
    })
    ac.insert({
        "address": "0x0000000000000000000000000000000000000002",
        "balance": "250000000000000000000",
    })

    # Seed synthetic transactions.
    tc = store_collection("transactions")
    tc.insert({
        "hash": _deterministic_hash("tx-seed-1"),
        "from": "0x0000000000000000000000000000000000000001",
        "to": "0x0000000000000000000000000000000000000002",
        "value": "1000000000000000000",
        "timeStamp": "1700000000",
        "gasUsed": "21000",
        "gasPrice": "1000000000",
        "isError": "0",
        "txreceipt_status": "1",
        "nonce": "0",
        "blockNumber": "1",
        "transactionIndex": "0",
        "input": "0x",
        "confirmations": "100",
    })
    tc.insert({
        "hash": _deterministic_hash("tx-seed-2"),
        "from": "0x0000000000000000000000000000000000000002",
        "to": "0x0000000000000000000000000000000000000000",
        "value": "2000000000000000000",
        "timeStamp": "1700000001",
        "gasUsed": "21000",
        "gasPrice": "1000000000",
        "isError": "0",
        "txreceipt_status": "1",
        "nonce": "0",
        "blockNumber": "2",
        "transactionIndex": "0",
        "input": "0x",
        "confirmations": "99",
    })

    # Seed synthetic contracts with ABI + source.
    cc = store_collection("contracts")
    cc.insert({
        "address": "0x0000000000000000000000000000000000000100",
        "ContractName": "MockToken",
        "CompilerVersion": "v0.8.20+commit.a1b79de6",
        "OptimizationUsed": "1",
        "Runs": "200",
        "ConstructorArguments": "",
        "EVMVersion": "Default",
        "Library": "",
        "LicenseType": "MIT",
        "Proxy": "0",
        "Implementation": "",
        "SwarmSource": "",
        "ABI": _mock_abi(),
        "SourceCode": _mock_source(),
    })
    cc.insert({
        "address": "0x0000000000000000000000000000000000000200",
        "ContractName": "MockNFT",
        "CompilerVersion": "v0.8.20+commit.a1b79de6",
        "OptimizationUsed": "1",
        "Runs": "200",
        "ConstructorArguments": "",
        "EVMVersion": "Default",
        "Library": "",
        "LicenseType": "MIT",
        "Proxy": "0",
        "Implementation": "",
        "SwarmSource": "",
        "ABI": _mock_nft_abi(),
        "SourceCode": _mock_nft_source(),
    })

    # Seed token holders.
    hc = store_collection("token_holders")
    hc.insert({
        "TokenHolderAddress": "0x0000000000000000000000000000000000000001",
        "TokenHolderQuantity": "500000000000000000000",
    })
    hc.insert({
        "TokenHolderAddress": "0x0000000000000000000000000000000000000002",
        "TokenHolderQuantity": "250000000000000000000",
    })

# --- synthetic contract data ---

# _mock_abi returns a minimal ERC-20-style ABI JSON string (synthetic).
def _mock_abi():
    return '[{"constant":true,"inputs":[],"name":"name","outputs":[{"name":"","type":"string"}],"type":"function"},{"constant":true,"inputs":[],"name":"symbol","outputs":[{"name":"","type":"string"}],"type":"function"},{"constant":true,"inputs":[],"name":"decimals","outputs":[{"name":"","type":"uint8"}],"type":"function"},{"constant":true,"inputs":[{"name":"owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"type":"function"},{"constant":false,"inputs":[{"name":"to","type":"address"},{"name":"value","type":"uint256"}],"name":"transfer","outputs":[{"name":"","type":"bool"}],"type":"function"}]'

# _mock_source returns a minimal ERC-20-style source (synthetic).
def _mock_source():
    return "// SPDX-License-Identifier: MIT\npragma solidity ^0.8.20;\ncontract MockToken {\n    string public name = 'Mock';\n    string public symbol = 'MOCK';\n}"

# _mock_nft_abi returns a minimal ERC-721-style ABI JSON string (synthetic).
def _mock_nft_abi():
    return '[{"constant":true,"inputs":[],"name":"name","outputs":[{"name":"","type":"string"}],"type":"function"},{"constant":true,"inputs":[{"name":"tokenId","type":"uint256"}],"name":"ownerOf","outputs":[{"name":"","type":"address"}],"type":"function"}]'

# _mock_nft_source returns a minimal ERC-721-style source (synthetic).
def _mock_nft_source():
    return "// SPDX-License-Identifier: MIT\npragma solidity ^0.8.20;\ncontract MockNFT {\n    string public name = 'Mock NFT';\n}"
