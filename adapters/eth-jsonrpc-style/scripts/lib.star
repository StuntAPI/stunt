# Shared library for eth-jsonrpc-style adapter scripts.
#
# This file is preloaded by stunt before each handler script. Its top-level
# definitions are available to all handlers as predeclared builtins.
#
# All hex values use Ethereum conventions: lowercase, "0x" prefixed.

# --- hex encoding helpers ---

# _to_hex converts a non-negative int to a "0x" prefixed hex string.
# e.g. _to_hex(255) == "0xff". Returns "0x0" for 0 or negative.
def _to_hex(n):
    if n == None:
        return "0x0"
    # Convert float to int (collections round-trip ints as floats via JSON).
    if type(n) == "float":
        n = int(n)
    if n < 0:
        return "0x0"
    if n == 0:
        return "0x0"
    digits = "0123456789abcdef"
    out = ""
    while n > 0:
        out = digits[n % 16] + out
        n = n // 16
    return "0x" + out

# _from_hex parses a hex string "0xNN" to an int. Returns 0 for None, empty,
# or non-hex input (never crashes).
def _from_hex(s):
    if s == None or s == "":
        return 0
    # Strip 0x prefix if present.
    if len(s) >= 2 and s[:2] == "0x":
        s = s[2:]
    if s == "":
        return 0
    digits = "0123456789abcdef"
    n = 0
    for i in range(len(s)):
        ch = s[i].lower()
        idx = -1
        for j in range(16):
            if digits[j] == ch:
                idx = j
                break
        if idx < 0:
            return 0
        n = n * 16 + idx
    return n

# --- JSON-RPC envelope helpers ---

# _rpc_ok builds a successful JSON-RPC 2.0 response.
def _rpc_ok(id, result):
    return {"jsonrpc": "2.0", "result": result, "id": id}

# _rpc_err builds an error JSON-RPC 2.0 response.
def _rpc_err(id, code, message):
    return {"jsonrpc": "2.0", "error": {"code": code, "message": message}, "id": id}

# --- deterministic hashing ---

# _deterministic_hash produces a 64-char hex string ("0x" + 64 hex chars)
# from an input string. This is NOT real keccak256 — it is a deterministic
# pseudo-hash suitable for a local mock where the same input always yields
# the same hash, and different inputs yield different hashes with very high
# probability. The output looks exactly like an Ethereum hash.
def _deterministic_hash(input_str):
    # Use a simple FNV-1a-like hash with multiple rounds to fill 64 hex chars
    # (32 bytes). We run 8 independent 32-bit hash lanes to produce 8*8=64
    # hex chars.
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
    out = "00000000"
    out_chars = []
    for _ in range(8):
        out_chars.append(digits[n % 16])
        n = n // 16
    # reverse
    result = ""
    for i in range(len(out_chars) - 1, -1, -1):
        result = result + out_chars[i]
    return result

# --- chain state seeding ---

# _seed initializes the deterministic chain state on first access:
#   - genesis block (block 0)
#   - seeded accounts with balances
#   - block number counter starts at 0
#   - nonce counter starts at 0 for each seeded account
def _seed():
    if store_kv_get("eth", "seeded") == "yes":
        return
    store_kv_set("eth", "seeded", "yes")

    # Block number starts at 0 (genesis).
    store_kv_set("eth", "block_number", "0")

    # Seed genesis block.
    genesis_hash = _deterministic_hash("genesis")
    bc = store_collection("blocks")
    bc.insert({
        "number": 0,
        "hash": genesis_hash,
        "parentHash": _deterministic_hash("genesis-parent"),
        "timestamp": "0x66e4f0a4",
        "transactions": [],
        "gasUsed": "0x0",
        "gasLimit": "0x1c9c380",
        "miner": "0x0000000000000000000000000000000000000000",
        "difficulty": "0x0",
        "nonce": "0x0000000000000000",
        "extraData": "0x",
        "sha3Uncles": _deterministic_hash("uncles-genesis"),
        "stateRoot": _deterministic_hash("state-genesis"),
        "logsBloom": "0x" + _hex64(),
    })

    # Seed well-known accounts with deterministic balances.
    # These addresses are synthetic (not real mainnet addresses).
    balances = {
        "0x0000000000000000000000000000000000000000": _to_hex(1000000000000000000000),
        "0x0000000000000000000000000000000000000001": _to_hex(500000000000000000000),
        "0x0000000000000000000000000000000000000002": _to_hex(250000000000000000000),
        "0x000000000000000000000000000000000000dEaD": _to_hex(0),
    }
    for addr in balances:
        store_kv_set("eth", "balance_" + addr.lower(), balances[addr])
        store_kv_set("eth", "nonce_" + addr.lower(), "0")

# _hex64 returns a 64-char zero string (for logsBloom placeholder).
def _hex64():
    return "0" * 64

# --- account helpers ---

# _get_balance returns the balance (hex string) for an address.
def _get_balance(addr):
    if addr == None:
        return "0x0"
    val = store_kv_get("eth", "balance_" + addr.lower())
    if val == None:
        return "0x0"
    return val

# _get_nonce returns the nonce (int) for an address.
def _get_nonce(addr):
    if addr == None:
        return 0
    val = store_kv_get("eth", "nonce_" + addr.lower())
    if val == None:
        return 0
    return _from_hex(val)

# _get_block_number returns the current block number (int).
def _get_block_number():
    val = store_kv_get("eth", "block_number")
    if val == None:
        return 0
    return _from_hex(val)

# _get_block_hash returns a deterministic block hash for a given block number.
def _get_block_hash(block_num):
    return _deterministic_hash("block-" + str(block_num))

# --- parameter extraction ---

# _first_param returns the first element of params, or None if empty/absent.
def _first_param(params):
    if params == None:
        return None
    if len(params) == 0:
        return None
    return params[0]

# _second_param returns the second element of params, or "latest" if absent.
def _second_param(params):
    if params == None or len(params) < 2:
        return "latest"
    return params[1]
