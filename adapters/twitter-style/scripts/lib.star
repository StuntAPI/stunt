# Shared helpers for twitter-style (preloaded into all handler scripts).

# _now returns the canonical synthetic timestamp for this adapter.
def _now():
    return "2024-01-15T12:00:00.000Z"

# _reverse returns a new list with elements in reverse order.
# Used for reverse-chronological tweet ordering (newest first).
def _reverse(lst):
    out = []
    for item in lst:
        out = [item] + out
    return out
