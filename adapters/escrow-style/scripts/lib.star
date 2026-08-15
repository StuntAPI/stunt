# Shared helpers, preloaded into every handler.

ESCROW_FEE_RATE = 325  # 3.25%, in basis points — escrow.com's general-merchandise rate

def next_id(ns):
    return store_kv_incr("ids", ns)

def body_of(req):
    """Parsed JSON body, or an empty dict."""
    b = req.get("body")
    if b == None:
        raw = req.get("raw_body", "")
        if raw == "":
            return {}
        return json.loads(raw)
    return b

def party_by_role(tx, role):
    for p in tx.get("parties", []):
        if p.get("role") == role:
            return p
    return None

def all_agreed(tx):
    for p in tx.get("parties", []):
        if not p.get("agreed", False):
            return False
    return True

def total_amount(tx):
    """Sum every schedule entry across items."""
    total = 0.0
    for item in tx.get("items", []):
        for s in item.get("schedule", []):
            total = total + float(s.get("amount", 0))
    return total

def is_secured(tx):
    """True once every schedule entry has been funded."""
    any_entry = False
    for item in tx.get("items", []):
        for s in item.get("schedule", []):
            any_entry = True
            if not s.get("status", {}).get("secured", False):
                return False
    return any_entry

def present(tx):
    """Return the API-shaped transaction: our ref_id surfaces as "id"."""
    out = {}
    for k in tx:
        if k != "ref_id" and k != "id":
            out[k] = tx[k]
    out["id"] = int(tx.get("ref_id", "0"))
    return out
