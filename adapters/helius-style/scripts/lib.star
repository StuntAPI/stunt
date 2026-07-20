# Shared library for helius-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _has_api_key checks for api-key in the query string.
def _has_api_key(req):
    key = req["query"].get("api-key", "")
    if key == "":
        return False
    return True

# _gen_signature generates a synthetic Solana transaction signature (base58-like).
def _gen_signature(seq):
    base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    s = ""
    v = seq
    if v == 0:
        v = 1
    for _ in range(88):
        s = s + base58[v % 58]
        v = v // 58
    return s

# _gen_blockhash generates a synthetic Solana blockhash.
def _gen_blockhash(seq):
    base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    s = ""
    v = seq + 1000000
    for _ in range(44):
        s = s + base58[v % 58]
        v = v // 58
    return s

# _balance_for_address returns a deterministic balance in lamports for an address.
def _balance_for_address(addr):
    h = 0
    for i in range(len(addr)):
        h = h * 31 + ord(addr[i])
    return (h % 1000000) * 1000000  # 0..~10^12 lamports

# _seed_tokens returns synthetic token balances for an address.
def _seed_tokens(addr):
    h = _balance_for_address(addr)
    return [
        {
            "mint": _hex_addr(h + 1),
            "amount": str(h % 1000000),
            "decimals": 6,
            "symbol": "USDC",
        },
        {
            "mint": _hex_addr(h + 2),
            "amount": str((h % 1000) * 1000000),
            "decimals": 9,
            "symbol": "SOL",
        },
    ]

# _seed_nfts returns synthetic NFT holdings for an address.
def _seed_nfts(addr):
    h = _balance_for_address(addr)
    return [
        {
            "mint": _hex_addr(h + 10),
            "name": "Synthetic NFT #" + str(h % 100),
            "symbol": "SNFT",
            "collection": {"key": _hex_addr(h + 20), "verified": True},
            "ownership": {"owner": addr, "verified": True},
        },
        {
            "mint": _hex_addr(h + 11),
            "name": "Degen Ape #" + str(h % 500),
            "symbol": "DAPE",
            "collection": {"key": _hex_addr(h + 21), "verified": True},
            "ownership": {"owner": addr, "verified": True},
        },
    ]

# _hex_addr generates a synthetic Solana address (base58, 44 chars).
def _hex_addr(n):
    base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    s = ""
    v = n
    if v == 0:
        v = 1
    for _ in range(44):
        s = s + base58[v % 58]
        v = v // 58
    return s
