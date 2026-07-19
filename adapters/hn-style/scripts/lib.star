# Shared library for hn-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# _strip_suffix removes a suffix from s. Returns s unchanged if the suffix is
# absent. Used to strip ".json" from captured route params.
def _strip_suffix(s, suffix):
    if s == None:
        return ""
    sl = len(s)
    fl = len(suffix)
    if sl >= fl and s[sl - fl:] == suffix:
        return s[:sl - fl]
    return s

# _id_from_param strips the ".json" suffix from a captured route param value.
def _id_from_param(raw):
    return _strip_suffix(raw, ".json")

# _cookie_value extracts a named cookie from the Cookie request header.
# Returns "" if the cookie is absent.
def _cookie_value(req, name):
    cookie_header = req["headers"].get("Cookie", "")
    if cookie_header == "":
        return ""
    for part in _split(cookie_header, ";"):
        part = _trim(part)
        eq = part.find("=")
        if eq < 0:
            continue
        k = part[:eq]
        v = part[eq + 1:]
        if k == name:
            return v
    return ""

# _session_user looks up the username for the current session cookie.
# Returns None if no valid session.
def _session_user(req):
    token = _cookie_value(req, "user")
    if token == "":
        return None
    sc = store_collection("sessions")
    doc = sc.get(token)
    if doc == None:
        return None
    return doc.get("username", "")

# _now returns a fixed epoch for deterministic synthetic timestamps.
def _now():
    return 1700000000

# _to_int parses a decimal string to int. Returns 0 for None, empty string,
# or any non-numeric input (never crashes on None).
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

# _split splits s on sep (single-char). Returns a list.
def _split(s, sep):
    parts = []
    current = ""
    for i in range(len(s)):
        ch = s[i]
        if ch == sep:
            parts.append(current)
            current = ""
        else:
            current = current + ch
    parts.append(current)
    return parts

# _trim removes leading/trailing whitespace.
def _trim(s):
    start = 0
    end = len(s)
    while start < end and (s[start] == " " or s[start] == "\t"):
        start = start + 1
    while end > start and (s[end - 1] == " " or s[end - 1] == "\t"):
        end = end - 1
    return s[start:end]
