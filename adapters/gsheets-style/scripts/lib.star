# Shared library for gsheets-style adapter scripts.
#
# This file is preloaded by stunt before each handler script in this
# directory. Its top-level definitions are available to all handlers as if
# they were builtins — without Starlark's load() (which stunt does not
# support). See internal/starlark/vm.go LoadWithLib.

# === Auth ===

# _bearer extracts the token from an "Authorization: Bearer <t>" header.
# Returns "" if the header is absent or not a Bearer header.
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer returns None if the bearer token is present (OK), or a 401
# response if missing. Google Sheets API requires an OAuth2 bearer token.
def _require_bearer(req):
    if _bearer(req) == "":
        return respond(401, {
            "error": {
                "code": 401,
                "message": "The request does not have valid authentication credentials.",
                "status": "UNAUTHENTICATED",
            },
        })
    return None

# === Google error envelope ===

# _g_err returns a Google-style error response.
def _g_err(code, message, status):
    return respond(code, {
        "error": {
            "code": code,
            "message": message,
            "status": status,
        },
    })

# _not_found returns a Google-style 404 error response.
def _not_found(msg):
    return _g_err(404, msg, "NOT_FOUND")

# _bad_request returns a Google-style 400 error response.
def _bad_request(msg):
    return _g_err(400, msg, "INVALID_ARGUMENT")

# === A1 notation parsing ===

# _is_alpha reports whether ch is an ASCII letter.
def _is_alpha(ch):
    return (ch >= "A" and ch <= "Z") or (ch >= "a" and ch <= "z")

# _is_digit reports whether ch is an ASCII digit.
def _is_digit(ch):
    return ch >= "0" and ch <= "9"

# _col_to_index converts column letters (e.g. "A", "Z", "AA") to a 0-indexed
# column number. A→0, B→1, ..., Z→25, AA→26, etc.
def _col_to_index(letters):
    result = 0
    for i in range(len(letters)):
        ch = letters[i].upper()
        result = result * 26 + (ord(ch) - ord("A") + 1)
    return result - 1

# _index_to_col converts a 0-indexed column number to A1 column letters.
# 0→"A", 25→"Z", 26→"AA", etc.
def _index_to_col(idx):
    letters = ""
    n = idx + 1
    while n > 0:
        n = n - 1
        letters = chr(ord("A") + n % 26) + letters
        n = n // 26
    return letters

# _parse_cell_ref parses a single cell reference like "B3" into (row, col)
# 0-indexed values. Returns None on parse failure.
def _parse_cell_ref(ref):
    col_str = ""
    row_str = ""
    in_row = False
    for i in range(len(ref)):
        ch = ref[i]
        if _is_digit(ch):
            in_row = True
            row_str = row_str + ch
        elif _is_alpha(ch):
            if in_row:
                return None  # letters after digits = bad
            col_str = col_str + ch
        else:
            return None
    if col_str == "" or row_str == "":
        return None
    return _to_int(row_str) - 1, _col_to_index(col_str)

# _parse_range parses an A1 notation range string into a dict with keys:
#   sheet  — sheet title (may be "")
#   row1   — start row (0-indexed)
#   col1   — start col (0-indexed)
#   row2   — end row (0-indexed, inclusive)
#   col2   — end col (0-indexed, inclusive)
# Handles:
#   "Sheet1!A1:C3"   → sheet="Sheet1", rows 0-2, cols 0-2
#   "Sheet1!A1"      → sheet="Sheet1", single cell
#   "A1:C3"          → no sheet
#   "A1"             → single cell, no sheet
def _parse_range(range_str):
    sheet = ""
    a1_part = range_str

    # Split off the sheet name if present.
    bang = range_str.find("!")
    if bang >= 0:
        sheet = range_str[:bang]
        a1_part = range_str[bang + 1:]

    # Handle full-column ranges like "A:C" by appending row 1 and max row.
    if a1_part.find(":") >= 0:
        parts = a1_part.split(":")
        start = parts[0]
        end = parts[1]
        # If start or end are column-only (e.g. "A" or "C"), expand.
        if _parse_cell_ref(start) != None:
            sr, sc = _parse_cell_ref(start)
        else:
            # column-only
            sc = _col_to_index(start)
            sr = 0
        if _parse_cell_ref(end) != None:
            er, ec = _parse_cell_ref(end)
        else:
            ec = _col_to_index(end)
            er = 999
    else:
        # Single cell.
        ref = _parse_cell_ref(a1_part)
        if ref == None:
            return None
        sr, sc = ref
        er, ec = sr, sc

    return {
        "sheet": sheet,
        "row1": sr,
        "col1": sc,
        "row2": er,
        "col2": ec,
    }

# _build_range_str constructs an A1 notation range string from a sheet title
# and row/col bounds (0-indexed, inclusive).
def _build_range_str(sheet, row1, col1, row2, col2):
    a1 = _index_to_col(col1) + str(row1 + 1) + ":" + _index_to_col(col2) + str(row2 + 1)
    if sheet != "":
        return sheet + "!" + a1
    return a1

# === Cell storage (KV-backed) ===

# _cell_key builds the KV key for a cell at (spreadsheetId, sheet, row, col).
def _cell_key(ss_id, sheet, row, col):
    return ss_id + "!" + sheet + "!" + str(row) + "!" + str(col)

# _get_cell reads a single cell value, or "" if empty.
def _get_cell(ss_id, sheet, row, col):
    v = store_kv_get("gsheets", _cell_key(ss_id, sheet, row, col))
    if v == None:
        return ""
    return v

# _set_cell writes a single cell value.
def _set_cell(ss_id, sheet, row, col, value):
    store_kv_set("gsheets", _cell_key(ss_id, sheet, row, col), value)

# === Utilities ===

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

# _seq generates a unique numeric id from a counter.
def _seq(name):
    return store_kv_incr("gsheets", name)

# _gen_spreadsheet_id generates a realistic Google Sheets spreadsheet ID.
# Real IDs look like "1ABC123...xyz" (44 base64url chars after the leading 1).
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

def _gen_spreadsheet_id(n):
    # Deterministic pseudo-ID from a sequence number.
    base = "1"
    val = n * 7919 + 104729
    for i in range(40):
        base = base + _B64URL[val % 64]
        val = val // 64 + 31
    return base[:20]

# _gen_sheet_id generates a numeric sheet ID.
def _gen_sheet_id(n):
    return n * 137 + 1000

# _default_sheet_title returns the title for the nth sheet.
def _default_sheet_title(n):
    if n == 0:
        return "Sheet1"
    return "Sheet" + str(n + 1)

# _seed ensures the default spreadsheet exists so basic GET operations work.
def _seed():
    if store_kv_get("gsheets", "seeded") == "yes":
        return
    store_kv_set("gsheets", "seeded", "yes")

    ss_id = _gen_spreadsheet_id(0)
    store_kv_set("gsheets", "default_ss_id", ss_id)

    sc = store_collection("spreadsheets")
    sc.insert({
        "spreadsheetId": ss_id,
        "properties": {
            "title": "Test Spreadsheet",
            "locale": "en_US",
            "autoRecalc": "ON_CHANGE",
            "timeZone": "America/Los_Angeles",
            "defaultFormat": {},
        },
        "sheets": [
            {
                "properties": {
                    "sheetId": _gen_sheet_id(0),
                    "title": "Sheet1",
                    "index": 0,
                    "gridProperties": {
                        "rowCount": 1000,
                        "columnCount": 26,
                    },
                },
            },
        ],
        "spreadsheetUrl": "https://docs.google.com/spreadsheets/d/" + ss_id + "/edit",
    })

    # Seed a few cells in the default spreadsheet.
    _set_cell(ss_id, "Sheet1", 0, 0, "Name")
    _set_cell(ss_id, "Sheet1", 0, 1, "Score")
    _set_cell(ss_id, "Sheet1", 1, 0, "Alice")
    _set_cell(ss_id, "Sheet1", 1, 1, "95")
    _set_cell(ss_id, "Sheet1", 2, 0, "Bob")
    _set_cell(ss_id, "Sheet1", 2, 1, "87")

# _find_ss looks up a spreadsheet by spreadsheetId from the collection.
# Returns the doc dict or None.
def _find_ss(ss_id):
    sc = store_collection("spreadsheets")
    for doc in sc.list():
        if doc.get("spreadsheetId") == ss_id:
            return doc
    return None
