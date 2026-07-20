# Shared library for erc4337-style adapter scripts.
#
# This file is preloaded by stunt before each handler script. Its top-level
# definitions are available to all handlers as predeclared builtins.

# --- constants ---

# The real v0.7 EntryPoint address on all major EVM chains.
ENTRY_POINT_V07 = "0x0000000071727De22E5E9d8BAf0edAc6f37da032"

# The required fields in a v0.7 UserOperation.
USEROP_FIELDS = [
    "sender",
    "nonce",
    "initCode",
    "callData",
    "callGasLimit",
    "verificationGasLimit",
    "preVerificationGas",
    "maxFeePerGas",
    "maxPriorityFeePerGas",
    "paymasterAndData",
    "signature",
]

# --- JSON-RPC envelope helpers ---

# _rpc_ok builds a successful JSON-RPC 2.0 response.
def _rpc_ok(id, result):
    return {"jsonrpc": "2.0", "result": result, "id": id}

# _rpc_err builds an error JSON-RPC 2.0 response.
def _rpc_err(id, code, message):
    return {"jsonrpc": "2.0", "error": {"code": code, "message": message}, "id": id}

# --- hex helpers ---

# _hex64 returns a 64-char zero string (for hex placeholders).
def _hex64():
    return "0" * 64

# _to_hex converts a non-negative int to a "0x" prefixed hex string.
def _to_hex(n):
    if n == None:
        return "0x0"
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

# --- deterministic hashing ---

# _deterministic_hash produces a 64-char hex string ("0x" + 64 hex chars)
# from an input string. Deterministic pseudo-hash, NOT real keccak256.
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

# --- parameter extraction ---

# _first_param returns the first element of params, or None if empty/absent.
def _first_param(params):
    if params == None:
        return None
    if len(params) == 0:
        return None
    return params[0]

# _second_param returns the second element of params, or None if absent.
def _second_param(params):
    if params == None or len(params) < 2:
        return None
    return params[1]

# --- userOp validation ---

# _validate_userop checks that a userOp has all required fields. Returns
# an error message string, or None if valid.
def _validate_userop(userop):
    if userop == None:
        return "missing userOperation"
    for i in range(len(USEROP_FIELDS)):
        field = USEROP_FIELDS[i]
        if field not in userop:
            return "missing required field: " + field
        val = userop[field]
        if val == None:
            return "null value for required field: " + field
    return None
