# Shared library for oneinch-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _token_list is a list of token info dicts (iterated for lookups).
_token_list = [
    {
        "address": "0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEe",
        "symbol": "ETH",
        "name": "Ether",
        "decimals": 18,
    },
    {
        "address": "0xA0b86991c6218b36c1D19D4a2e9Eb0cE3606eB48",
        "symbol": "USDC",
        "name": "USD Coin",
        "decimals": 6,
    },
    {
        "address": "0xdAC17F958D2ee523a2206206994597C13D831ec7",
        "symbol": "USDT",
        "name": "Tether USD",
        "decimals": 6,
    },
    {
        "address": "0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599",
        "symbol": "WBTC",
        "name": "Wrapped BTC",
        "decimals": 8,
    },
    {
        "address": "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
        "symbol": "WETH",
        "name": "Wrapped Ether",
        "decimals": 18,
    },
    {
        "address": "0x1f9840a85d5aF5bf1D1762F925BDADdC4201F984",
        "symbol": "UNI",
        "name": "Uniswap",
        "decimals": 18,
    },
]

# _token_info looks up token metadata by address (case-insensitive).
def _token_info(addr):
    target = addr.lower()
    for t in _token_list:
        if t["address"].lower() == target:
            return t
    return None

# _to_int parses a decimal string to int. Returns 0 for None/empty.
def _to_int(s):
    if s == None or s == "":
        return 0
    n = 0
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
        else:
            return 0
    return n

# _compute_quote returns a deterministic toAmount based on src/dst token
# decimals and input amount.
def _compute_quote(src_info, dst_info, amount):
    src_decimals = src_info["decimals"]
    dst_decimals = dst_info["decimals"]

    rate = _deterministic_rate(src_info["symbol"], dst_info["symbol"])

    amt = _to_int(amount)
    to_amt = amt * rate
    if dst_decimals > src_decimals:
        for _ in range(dst_decimals - src_decimals):
            to_amt = to_amt * 10
    elif src_decimals > dst_decimals:
        for _ in range(src_decimals - dst_decimals):
            to_amt = to_amt // 10

    return str(to_amt)

# _deterministic_rate produces a pseudo-rate from two symbols.
def _deterministic_rate(sym_a, sym_b):
    h = 1
    for i in range(len(sym_a)):
        h = h * 31 + ord(sym_a[i])
    for i in range(len(sym_b)):
        h = h * 31 + ord(sym_b[i])
    return 1000 + (h % 2001)

# _protocols returns a deterministic list of DEX protocols for the route.
def _protocols(src_sym, dst_sym):
    h = _deterministic_rate(src_sym, dst_sym)
    p1 = 40 + (h % 30)
    p2 = 100 - p1
    return [
        {"name": "UNISWAP_V3", "part": str(p1)},
        {"name": "SUSHISWAP", "part": str(p2)},
    ]

# _SPENDER is the synthetic 1inch router address.
_SPENDER = "0x1111111254EEB25477B68fb85Ed929f73A960582"

# _ROUTER is the synthetic 1inch router contract (for swap calldata).
_ROUTER = "0x1111111254EEB25477B68fb85Ed929f73A960582"

# _fake_calldata generates a deterministic hex calldata blob.
def _fake_calldata(prefix, seed):
    base = "0x12e7c2a" + prefix + "000000000000000000000000"
    return base + _hex_seed(seed)

# _hex_seed converts a number to a 40-char hex string.
def _hex_seed(n):
    hexchars = "0123456789abcdef"
    s = ""
    v = n
    if v == 0:
        v = 1
    for _ in range(40):
        s = s + hexchars[v % 16]
        v = v // 16
    if v > 0:
        s = s + "00000000"
    return s[:40]
