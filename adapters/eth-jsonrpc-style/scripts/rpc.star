# JSON-RPC 2.0 handler — dispatches all eth_* / web3_* / net_* methods.
#
# POST / → {jsonrpc:"2.0", method:..., params:[...], id:...}
#
# Both single requests ({...}) and batch requests ([{...},{...}]) are
# supported. For batch, the response is an array of results.
#
# The chain is DETERMINISTIC and STATEFUL:
#   - eth_sendRawTransaction mints a hash, bumps the block number, records
#     logs, and increments the sender's nonce — so a subsequent
#     eth_getTransactionReceipt returns the receipt with logs.
#   - eth_getLogs returns the logs from sent transactions.

# Shared helpers are preloaded from scripts/lib.star.

# on_jsonrpc is the single entry point for all JSON-RPC requests.
def on_jsonrpc(req):
    _seed()

    body = req["body"]
    if body == None:
        return respond(200, _rpc_err(None, -32600, "Invalid Request"))

    # Detect batch: array bodies arrive wrapped under "_batch" by the engine.
    batch = None
    if "method" in body:
        # Single request.
        return respond(200, _dispatch(body))
    elif "_batch" in body:
        batch = body["_batch"]
    else:
        return respond(200, _rpc_err(None, -32600, "Invalid Request"))

    # Process batch: map each request and collect results.
    results = []
    for item in batch:
        results.append(_dispatch(item))
    return respond(200, results)

# _dispatch handles a single JSON-RPC request object and returns a response.
def _dispatch(rpc):
    if rpc == None:
        return _rpc_err(None, -32600, "Invalid Request")

    method = rpc.get("method", "")
    if method == None:
        method = ""
    params = rpc.get("params", [])
    if params == None:
        params = []
    id = rpc.get("id", None)

    if method == "":
        return _rpc_err(id, -32600, "Invalid Request")

    # Dispatch table.
    if method == "eth_chainId":
        return _rpc_ok(id, "0x1")
    elif method == "eth_blockNumber":
        return _rpc_ok(id, _to_hex(_get_block_number()))
    elif method == "eth_gasPrice":
        return _rpc_ok(id, "0x3b9aca00")
    elif method == "net_version":
        return _rpc_ok(id, "1")
    elif method == "web3_clientVersion":
        return _rpc_ok(id, "Stunt/v1.0.0/mock")
    elif method == "eth_getBalance":
        return _handle_get_balance(id, params)
    elif method == "eth_getTransactionCount":
        return _handle_get_tx_count(id, params)
    elif method == "eth_sendRawTransaction":
        return _handle_send_raw_tx(id, params)
    elif method == "eth_getTransactionReceipt":
        return _handle_get_receipt(id, params)
    elif method == "eth_getTransactionByHash":
        return _handle_get_tx_by_hash(id, params)
    elif method == "eth_call":
        return _handle_call(id, params)
    elif method == "eth_getLogs":
        return _handle_get_logs(id, params)
    elif method == "eth_getBlockByNumber":
        return _handle_get_block_by_number(id, params)
    elif method == "eth_estimateGas":
        return _rpc_ok(id, "0x5208")
    elif method == "eth_getCode":
        return _rpc_ok(id, "0x")
    elif method == "eth_getStorageAt":
        return _rpc_ok(id, "0x" + _hex64())
    elif method == "eth_getBlockByHash":
        return _handle_get_block_by_hash(id, params)
    elif method == "eth_feeHistory":
        return _handle_fee_history(id, params)
    elif method == "eth_maxPriorityFeePerGas":
        return _rpc_ok(id, "0x1")
    elif method == "net_listening":
        return _rpc_ok(id, True)
    elif method == "net_peerCount":
        return _rpc_ok(id, "0x0")
    elif method == "eth syncing":
        return _rpc_ok(id, False)
    elif method == "eth_accounts":
        return _rpc_ok(id, [])
    elif method == "eth_getBlockTransactionCountByNumber":
        return _rpc_ok(id, "0x0")
    else:
        return _rpc_err(id, -32601, "the method " + method + " does not exist/is not available")

# --- method handlers ---

def _handle_get_balance(id, params):
    addr = _first_param(params)
    return _rpc_ok(id, _get_balance(addr))

def _handle_get_tx_count(id, params):
    addr = _first_param(params)
    return _rpc_ok(id, _to_hex(_get_nonce(addr)))

def _handle_send_raw_tx(id, params):
    raw_tx = _first_param(params)
    if raw_tx == None:
        return _rpc_err(id, -32000, "missing raw transaction")

    # Deterministic tx hash from the raw transaction data.
    tx_hash = _deterministic_hash(raw_tx)

    # Bump block number — each transaction mines a new block.
    block_num = _get_block_number() + 1
    store_kv_set("eth", "block_number", _to_hex(block_num))

    block_hash = _get_block_hash(block_num)

    # Derive a deterministic sender and receiver from the hash.
    # (We don't parse the RLP — just use the hash for deterministic values.)
    sender = "0x" + tx_hash[2:42]
    receiver = "0x" + tx_hash[42:82]

    # Increment sender nonce.
    sender_nonce = _get_nonce(sender) + 1
    store_kv_set("eth", "nonce_" + sender.lower(), _to_hex(sender_nonce))

    # Create a deterministic log for this transaction.
    log_address = receiver
    log_topics = [_deterministic_hash("topic-" + tx_hash)]
    log_data = "0x" + tx_hash[2:66]

    log_entry = {
        "address": log_address,
        "topics": log_topics,
        "data": log_data,
        "blockNumber": _to_hex(block_num),
        "transactionHash": tx_hash,
        "transactionIndex": "0x0",
        "logIndex": "0x0",
        "removed": False,
    }

    # Store the transaction.
    txc = store_collection("transactions")
    txc.insert({
        "hash": tx_hash,
        "blockNumber": _to_hex(block_num),
        "blockHash": block_hash,
        "from": sender,
        "to": receiver,
        "gas": "0x5208",
        "gasPrice": "0x3b9aca00",
        "input": "0x",
        "nonce": _to_hex(sender_nonce - 1),
        "transactionIndex": "0x0",
        "value": "0xde0b6b3a7640000",
        "type": "0x0",
        "chainId": "0x1",
    })

    # Store the receipt (STATEFUL — a sent tx has a receipt).
    rc = store_collection("receipts")
    rc.insert({
        "transactionHash": tx_hash,
        "blockNumber": _to_hex(block_num),
        "blockHash": block_hash,
        "gasUsed": "0x5208",
        "cumulativeGasUsed": "0x5208",
        "effectiveGasPrice": "0x3b9aca00",
        "status": "0x1",
        "from": sender,
        "to": receiver,
        "transactionIndex": "0x0",
        "type": "0x0",
        "logs": [log_entry],
        "logsBloom": "0x" + _hex64(),
        "contractAddress": None,
    })

    # Store the log for eth_getLogs.
    lc = store_collection("logs")
    lc.insert(log_entry)

    # Store/update the block with this transaction.
    bc = store_collection("blocks")
    # Try to update the current block's transactions, or insert a new block.
    existing = None
    for blk in bc.list():
        if blk.get("number", -1) == block_num:
            existing = blk
            break
    if existing != None:
        txs = existing.get("transactions", [])
        txs.append(tx_hash)
        bc.update(existing["id"], {
            "number": block_num,
            "hash": block_hash,
            "parentHash": _get_block_hash(block_num - 1),
            "timestamp": "0x66e4f0a4",
            "transactions": txs,
            "gasUsed": _to_hex(len(txs) * 0x5208),
            "gasLimit": "0x1c9c380",
            "miner": sender,
            "difficulty": "0x0",
            "nonce": "0x0000000000000000",
            "extraData": "0x",
            "sha3Uncles": _deterministic_hash("uncles-" + str(block_num)),
            "stateRoot": _deterministic_hash("state-" + str(block_num)),
            "logsBloom": "0x" + _hex64(),
        })
    else:
        bc.insert({
            "number": block_num,
            "hash": block_hash,
            "parentHash": _get_block_hash(block_num - 1),
            "timestamp": "0x66e4f0a4",
            "transactions": [tx_hash],
            "gasUsed": "0x5208",
            "gasLimit": "0x1c9c380",
            "miner": sender,
            "difficulty": "0x0",
            "nonce": "0x0000000000000000",
            "extraData": "0x",
            "sha3Uncles": _deterministic_hash("uncles-" + str(block_num)),
            "stateRoot": _deterministic_hash("state-" + str(block_num)),
            "logsBloom": "0x" + _hex64(),
        })

    return _rpc_ok(id, tx_hash)

def _handle_get_receipt(id, params):
    tx_hash = _first_param(params)
    if tx_hash == None:
        return _rpc_err(id, -32000, "missing tx hash")

    rc = store_collection("receipts")
    for r in rc.list():
        if r.get("transactionHash", "") == tx_hash:
            return _rpc_ok(id, r)

    # No receipt found.
    return _rpc_ok(id, None)

def _handle_get_tx_by_hash(id, params):
    tx_hash = _first_param(params)
    if tx_hash == None:
        return _rpc_ok(id, None)

    txc = store_collection("transactions")
    for tx in txc.list():
        if tx.get("hash", "") == tx_hash:
            return _rpc_ok(id, tx)

    return _rpc_ok(id, None)

def _handle_call(id, params):
    # eth_call({to, data}, tag) → deterministic hex result.
    # We don't execute bytecode — just return plausible hex based on data.
    call_obj = _first_param(params)
    if call_obj == None:
        return _rpc_ok(id, "0x")

    data = ""
    if "data" in call_obj:
        data = call_obj["data"]
    if data == None:
        data = ""

    # If the data starts with a function selector (0x + 8 hex), echo it
    # back as a plausible return value.
    if len(data) >= 10 and data[:2] == "0x":
        selector = data[2:10]
        return _rpc_ok(id, "0x" + selector + _hex64())

    return _rpc_ok(id, "0x")

def _handle_get_logs(id, params):
    filter_obj = _first_param(params)
    if filter_obj == None:
        filter_obj = {}

    from_block = 0
    to_block = 999999999
    if "fromBlock" in filter_obj and filter_obj["fromBlock"] != None:
        fb = filter_obj["fromBlock"]
        if fb == "latest":
            from_block = _get_block_number()
        elif fb == "earliest":
            from_block = 0
        else:
            from_block = _from_hex(fb)
    if "toBlock" in filter_obj and filter_obj["toBlock"] != None:
        tb = filter_obj["toBlock"]
        if tb == "latest":
            to_block = _get_block_number()
        elif tb == "earliest":
            to_block = 0
        else:
            to_block = _from_hex(tb)

    address_filter = None
    if "address" in filter_obj and filter_obj["address"] != None:
        address_filter = filter_obj["address"]

    topics_filter = None
    if "topics" in filter_obj and filter_obj["topics"] != None:
        topics_filter = filter_obj["topics"]

    lc = store_collection("logs")
    result = []
    for log in lc.list():
        log_block = _from_hex(log.get("blockNumber", "0x0"))
        if log_block < from_block or log_block > to_block:
            continue

        # Filter by address.
        if address_filter != None:
            if _matches_address(address_filter, log.get("address", "")):
                pass
            else:
                continue

        # Filter by topics (positional matching).
        if topics_filter != None:
            if _matches_topics(topics_filter, log.get("topics", [])):
                pass
            else:
                continue

        result.append(log)

    return _rpc_ok(id, result)

def _handle_get_block_by_number(id, params):
    tag = _first_param(params)
    full_tx = _second_param(params)
    if full_tx == None:
        full_tx = False

    if tag == None or tag == "latest":
        block_num = _get_block_number()
    elif tag == "earliest":
        block_num = 0
    elif tag == "pending":
        block_num = _get_block_number()
    else:
        block_num = _from_hex(tag)

    bc = store_collection("blocks")
    for blk in bc.list():
        if blk.get("number", -1) == block_num:
            return _rpc_ok(id, _block_response(blk, full_tx))

    # Block not found — return null.
    return _rpc_ok(id, None)

def _handle_get_block_by_hash(id, params):
    block_hash = _first_param(params)
    full_tx = _second_param(params)
    if full_tx == None:
        full_tx = False

    bc = store_collection("blocks")
    for blk in bc.list():
        if blk.get("hash", "") == block_hash:
            return _rpc_ok(id, _block_response(blk, full_tx))

    return _rpc_ok(id, None)

def _handle_fee_history(id, params):
    # eth_feeHistory(blockCount, newestBlock, rewardPercentiles)
    block_count = 1
    if len(params) > 0:
        block_count = _from_hex(params[0])
    if block_count == 0:
        block_count = 1

    current = _get_block_number()
    oldest = current - block_count + 1
    if oldest < 0:
        oldest = 0

    base_fees = []
    gas_used_ratios = []
    for i in range(oldest, current + 1):
        base_fees.append("0x7")
        gas_used_ratios.append(0.5)

    # Next base fee after the range.
    base_fees.append("0x7")

    return _rpc_ok(id, {
        "oldestBlock": _to_hex(oldest),
        "baseFeePerGas": base_fees,
        "gasUsedRatio": gas_used_ratios,
    })

# --- helpers ---

# _block_response builds a block response object. If full_tx is True,
# transactions are expanded to full objects; otherwise they are just hashes.
def _block_response(blk, full_tx):
    txs_field = "transactions"
    txs = blk.get(txs_field, [])
    if full_tx:
        # Expand tx hashes to full transaction objects.
        txc = store_collection("transactions")
        expanded = []
        for tx_hash in txs:
            for tx in txc.list():
                if tx.get("hash", "") == tx_hash:
                    expanded.append(tx)
                    break
        txs = expanded

    return {
        "number": _to_hex(blk.get("number", 0)),
        "hash": blk.get("hash", ""),
        "parentHash": blk.get("parentHash", ""),
        "nonce": blk.get("nonce", "0x0000000000000000"),
        "sha3Uncles": blk.get("sha3Uncles", ""),
        "logsBloom": blk.get("logsBloom", "0x" + _hex64()),
        "transactionsRoot": _deterministic_hash("txroot-" + str(blk.get("number", 0))),
        "stateRoot": blk.get("stateRoot", ""),
        "receiptsRoot": _deterministic_hash("rcroot-" + str(blk.get("number", 0))),
        "miner": blk.get("miner", "0x0000000000000000000000000000000000000000"),
        "difficulty": blk.get("difficulty", "0x0"),
        "totalDifficulty": blk.get("difficulty", "0x0"),
        "extraData": blk.get("extraData", "0x"),
        "size": "0x3e8",
        "gasLimit": blk.get("gasLimit", "0x1c9c380"),
        "gasUsed": blk.get("gasUsed", "0x0"),
        "timestamp": blk.get("timestamp", "0x0"),
        "transactions": txs,
        "uncles": [],
    }

# _matches_address checks if a log address matches a filter. The filter can
# be a single string or a list of strings.
def _matches_address(filter, addr):
    if _is_list(filter):
        for a in filter:
            if a.lower() == addr.lower():
                return True
        return False
    return filter.lower() == addr.lower()

# _matches_topics checks positional topic matching. filter[i] matches
# log_topics[i]; None in the filter matches any topic at that position.
def _matches_topics(filter, log_topics):
    for i in range(len(filter)):
        wanted = filter[i]
        if wanted == None:
            continue
        if i >= len(log_topics):
            return False
        if wanted != log_topics[i]:
            return False
    return True

# _is_list returns True if v is a Starlark list.
def _is_list(v):
    return type(v) == type([])
