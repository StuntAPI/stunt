# Shared library for thegraph-style adapter scripts.
#
# This file is preloaded by stunt before each handler script. Its top-level
# definitions are available to all handlers as predeclared builtins.

# --- seeded subgraph IDs ---

# Uniswap V3 style subgraph.
SUBGRAPH_UNISWAP_V3 = "5zvR82QoaXYxfyKOCH8Qfl6pUCWd7YFXq56Y3ZSDXx2W"
# ENS style subgraph.
SUBGRAPH_ENS = "5XqPmWe6gZyrTtFjASCbxgykJ7KbAA8puFezV8vsJoEB"

# --- string helpers ---

# _contains checks if a string contains a substring.
def _contains(s, sub):
    if s == None or sub == None:
        return False
    if len(sub) == 0:
        return True
    if len(sub) > len(s):
        return False
    for i in range(len(s) - len(sub) + 1):
        match = True
        for j in range(len(sub)):
            if s[i + j] != sub[j]:
                match = False
                break
        if match:
            return True
    return False

# _to_int parses a decimal string to int. Returns 0 for None, empty, or
# non-numeric input.
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

# _str converts any value to string (handles None and numbers).
def _str(v):
    if v == None:
        return ""
    if type(v) == "int" or type(v) == "float":
        # Starlark's str() works, but we use the conversion for safety.
        return _int_to_str(int(v))
    return v

# _int_to_str converts an int to a decimal string.
def _int_to_str(n):
    if n == 0:
        return "0"
    digits = "0123456789"
    out = ""
    while n > 0:
        out = digits[n % 10] + out
        n = n // 10
    return out

# --- GraphQL query parsing ---

# _extract_fields extracts the list of requested field names from a GraphQL
# query fragment like "pools(first:5, orderBy:volumeUSD){id token0{symbol} token1{symbol} totalValueLockedUSD}".
# Returns a list of top-level field names (without nesting).
def _extract_fields(fragment):
    fields = []
    i = 0
    depth = 0
    current = ""
    started = False
    while i < len(fragment):
        ch = fragment[i]
        if ch == "{":
            if depth == 0 and started and current != "":
                fields.append(_trim(current))
            depth = depth + 1
            current = ""
            started = True
        elif ch == "}":
            depth = depth - 1
            if depth == 0:
                # Next entity or end.
                pass
            current = ""
            started = False
        elif depth > 0 and started:
            if ch == "," or ch == "\n" or ch == " " or ch == "\t":
                if current != "":
                    fields.append(_trim(current))
                    current = ""
            else:
                current = current + ch
        else:
            current = current + ch
        i = i + 1
    # Catch trailing field.
    if current != "" and depth == 0:
        fields.append(_trim(current))
    return fields

# _trim removes leading/trailing whitespace from a string.
def _trim(s):
    start = 0
    end = len(s)
    while start < end:
        ch = s[start]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            start = start + 1
        else:
            break
    while end > start:
        ch = s[end - 1]
        if ch == " " or ch == "\t" or ch == "\n" or ch == "\r":
            end = end - 1
        else:
            break
    return s[start:end]

# _extract_arg_int extracts an integer argument from a GraphQL field header.
# e.g. from "pools(first:5, orderBy:volumeUSD)" with key="first" → 5.
def _extract_arg_int(header, key):
    pattern = key + ":"
    idx = _find_str(header, pattern)
    if idx < 0:
        return 0
    val_start = idx + len(pattern)
    val = ""
    for i in range(val_start, len(header)):
        ch = header[i]
        if ch == "," or ch == ")" or ch == " ":
            break
        val = val + ch
    return _to_int(_trim(val))

# _extract_arg_str extracts a string argument from a GraphQL field header.
# e.g. from "pools(orderBy:volumeUSD)" with key="orderBy" → "volumeUSD".
def _extract_arg_str(header, key):
    pattern = key + ":"
    idx = _find_str(header, pattern)
    if idx < 0:
        return ""
    val_start = idx + len(pattern)
    val = ""
    for i in range(val_start, len(header)):
        ch = header[i]
        if ch == "," or ch == ")" or ch == " ":
            break
        val = val + ch
    return _trim(val)

# _find_str finds the index of a substring, or -1.
def _find_str(s, sub):
    if len(sub) == 0:
        return 0
    for i in range(len(s) - len(sub) + 1):
        match = True
        for j in range(len(sub)):
            if s[i + j] != sub[j]:
                match = False
                break
        if match:
            return i
    return -1

# _has_field checks if a field name appears in the GraphQL query fragment.
def _has_field(query, field):
    return _contains(query, field)
