# Shared library for gdocs-style adapter scripts.
#
# Implements the Google Docs structural content model:
#
#   document -> model: list of paragraphs
#   paragraph -> {"elements": [...], "style": {...}, "bullet": None|{...}}
#   element  -> {"t": "text", "text": str, "style": {...}}
#             | {"t": "brk"}                              (page break)
#             | {"t": "obj", "objectId": str}             (inline image)
#
# Every paragraph implicitly ends with one "\n" that belongs to the paragraph
# (its endIndex covers it). All indices exposed by the API are 1-based UTF-16
# code units, exactly like the real service: an astral character (e.g. an
# emoji, encoded as 4 UTF-8 bytes) occupies TWO consecutive indices (its
# UTF-16 surrogate pair), while a BMP character occupies one. An index that
# falls strictly between the high and low halves of a surrogate pair is
# invalid and is rejected with 400 INVALID_ARGUMENT.
#
# The model is persisted on the document record under "model"; the ranged
# body.content shape the API returns is derived from it on read.

_PB = "\u000c"  # internal stand-in for a page break (1 UTF-16 unit)
_OBJ = "\ufffc"  # internal stand-in for an inline object (1 UTF-16 unit)

# --- auth / errors ---------------------------------------------------------

# _bearer extracts the token from "Authorization: Bearer <t>".
def _bearer(req):
    auth = req["headers"].get("Authorization", "")
    if auth[:7] == "Bearer ":
        return auth[7:]
    return ""

# _require_bearer returns None if OK, or a 401 response if missing.
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

# _g_err returns a Google-style error response.
def _g_err(code, message, status):
    return respond(code, {
        "error": {
            "code": code,
            "message": message,
            "status": status,
        },
    })

# _bad_request returns a 400 INVALID_ARGUMENT error response.
def _bad_request(message):
    return _g_err(400, message, "INVALID_ARGUMENT")

# --- clock -----------------------------------------------------------------

# _now_ms returns the current time in the Docs revision timestamp format
# (RFC3339 with milliseconds, e.g. "...T00:00:00.000Z"). Derived from the
# engine clock, so revision modifiedTime values track real elapsed time.
def _now_ms():
    rfc = clock.now_rfc3339()
    return rfc[:-1] + ".000Z"

# _touch_doc records a revision on the document: appends
# {id, modifiedTime, lastModifier} to doc["revisions"] (seeded with the
# creation revision on first touch). Every batchUpdate calls this, so the
# revisions endpoint reflects the document's real edit history.
def _touch_doc(doc):
    revs = doc.get("revisions")
    if revs == None:
        revs = [{
            "id": "1",
            "modifiedTime": _now_ms(),
            "lastModifier": {"displayName": "Test User", "me": True},
        }]
    else:
        revs = list(revs)
        revs.append({
            "id": str(len(revs) + 1),
            "modifiedTime": _now_ms(),
            "lastModifier": {"displayName": "Test User", "me": True},
        })
    doc["revisions"] = revs
    return doc

# --- numeric coercion ------------------------------------------------------
# JSON bodies deliver every number as a float (2, not 2.0, never arrives).
# _num_to_int coerces a body value to an int, falling back to dflt.

def _num_to_int(v, dflt):
    if v == None:
        return dflt
    t = type(v)
    if t == "int":
        return v
    if t == "float":
        return int(v)
    if t == "bool":
        return dflt
    s = str(v)
    n = 0
    seen = False
    for i in range(len(s)):
        ch = s[i]
        if ch >= "0" and ch <= "9":
            n = n * 10 + (ord(ch) - ord("0"))
            seen = True
        else:
            break
    if not seen:
        return dflt
    return n

# --- UTF-16 index arithmetic -----------------------------------------------

# _u16_len returns the length of s in UTF-16 code units (bytes are decoded as
# UTF-8; malformed tails are treated as single units so len() never panics).
def _u16_len(s):
    n = 0
    i = 0
    L = len(s)
    while i < L:
        b = ord(s[i])
        if b < 128:
            i += 1
            n += 1
        elif b < 224:
            i += 2
            n += 1
        elif b < 240:
            i += 3
            n += 1
        else:
            i += 4
            n += 2
    if i > L:
        # truncated sequence at the tail: count the leftover bytes as units
        n = n - (i - L)
    return n

# _u16_off maps a UTF-16 offset t into s. Returns [byte_offset, splits] where
# splits is True when t lands strictly inside a surrogate pair (only possible
# for astral characters, which span two units).
def _u16_off(s, t):
    i = 0
    n = 0
    L = len(s)
    while i < L and n < t:
        b = ord(s[i])
        if b < 128:
            step = 1
            units = 1
        elif b < 224:
            step = 2
            units = 1
        elif b < 240:
            step = 3
            units = 1
        else:
            step = 4
            units = 2
        if i + step > L:
            step = 1
            units = 1
        if n + units > t:
            return [i, True]
        n += units
        i += step
    return [i, False]

# --- model construction ----------------------------------------------------

def _new_text_el(text, style):
    return {"t": "text", "text": text, "style": style}

def _new_para(style, bullet):
    if style == None:
        style = {"namedStyle": "NORMAL_TEXT"}
    return {"elements": [], "style": style, "bullet": bullet}

def _copy_style(style):
    if style == None:
        return {}
    out = {}
    for k in style:
        out[k] = style[k]
    return out

def _copy_el(el):
    if el["t"] != "text":
        return {"t": el["t"], "objectId": el.get("objectId", "")}
    return _new_text_el(el["text"], _copy_style(el["style"]))

def _el_u16_len(el):
    if el["t"] == "text":
        return _u16_len(el["text"])
    return 1

# _para_vtext returns the paragraph's virtual text: element texts with
# stand-in characters for page breaks / inline objects, plus the trailing
# newline that belongs to the paragraph.
def _para_vtext(para):
    s = ""
    for el in para["elements"]:
        if el["t"] == "text":
            s += el["text"]
        elif el["t"] == "brk":
            s += _PB
        else:
            s += _OBJ
    return s + "\n"

def _model_text(model):
    s = ""
    for para in model:
        s += _para_vtext(para)
    return s

def _style_same(a, b):
    return json.encode(a) == json.encode(b)

# _normalize re-splits paragraphs after a mutation: any text element that
# contains "\n" is broken into separate paragraphs. Continuation paragraphs
# inherit the source paragraph's style and bullet (pressing Enter in a
# bulleted HEADING_1 paragraph continues the list in the docs UI).
def _normalize(model):
    out = []
    for para in model:
        cur = _new_para(_copy_style(para["style"]), para["bullet"])
        for el in para["elements"]:
            if el["t"] != "text":
                cur["elements"].append(el)
                continue
            parts = el["text"].split("\n")
            for j in range(len(parts)):
                if j > 0:
                    out.append(cur)
                    cur = _new_para(_copy_style(para["style"]), para["bullet"])
                if parts[j] != "":
                    cur["elements"].append(_new_text_el(parts[j], _copy_style(el["style"])))
        out.append(cur)
    return out

def _replace_model(model, newmodel):
    while len(model) > 0:
        model.pop()
    for p in newmodel:
        model.append(p)

# --- locating an insertion point -------------------------------------------
# _locate_point maps a 1-based document index to [status, para_idx, elem_idx,
# byte_off]. status is "ok", "split" (index inside a surrogate pair) or "out".
# An index pointing at a paragraph's trailing newline means "insert just
# before the newline" (i.e. append to that paragraph).

def _locate_point(model, index):
    pos = 1
    for pi in range(len(model)):
        para = model[pi]
        vtext = _para_vtext(para)
        plen = _u16_len(vtext)
        if index >= pos and index <= pos + plen - 1:
            local = index - pos
            ep = 0
            els = para["elements"]
            for ei in range(len(els)):
                el = els[ei]
                elen = _el_u16_len(el)
                if local < ep + elen:
                    if el["t"] == "text" and local > ep:
                        r = _u16_off(el["text"], local - ep)
                        if r[1]:
                            return ["split", pi, ei, r[0]]
                        return ["ok", pi, ei, r[0]]
                    if local == ep:
                        return ["ok", pi, ei, 0]
                    return ["ok", pi, ei + 1, 0]
                ep += elen
            return ["ok", pi, len(els), 0]
        pos += plen
    return ["out", 0, 0, 0]

# --- structural mutations ---------------------------------------------------
# Each _apply_* mutates the model in place and returns None on success or an
# error message for a 400 INVALID_ARGUMENT response.

def _apply_insert_text(model, index, text):
    total = _u16_len(_model_text(model))
    if index < 1 or index > total:
        return "location.index " + str(index) + " is out of range [1, " + str(total) + "]."
    loc = _locate_point(model, index)
    if loc[0] == "split":
        return "location.index " + str(index) + " falls inside a UTF-16 surrogate pair."
    pi = loc[1]
    ei = loc[2]
    off = loc[3]
    para = model[pi]
    els = para["elements"]
    if ei < len(els) and els[ei]["t"] == "text":
        el = els[ei]
        if off == 0:
            if ei > 0 and els[ei - 1]["t"] == "text" and _style_same(els[ei - 1]["style"], el["style"]):
                els[ei - 1]["text"] += text
            else:
                els.insert(ei, _new_text_el(text, _copy_style(el["style"])))
        else:
            head = el["text"][:off]
            tail = el["text"][off:]
            el["text"] = head + text
            if tail != "":
                els.insert(ei + 1, _new_text_el(tail, _copy_style(el["style"])))
    elif ei >= len(els) and len(els) > 0 and els[len(els) - 1]["t"] == "text":
        els[len(els) - 1]["text"] += text
    else:
        els.insert(ei, _new_text_el(text, {}))
    _replace_model(model, _normalize(model))
    return None

# _apply_insert_element inserts a non-text element (page break / inline
# object reference) at the given index.
def _apply_insert_element(model, index, el):
    total = _u16_len(_model_text(model))
    if index < 1 or index > total:
        return "location.index " + str(index) + " is out of range [1, " + str(total) + "]."
    loc = _locate_point(model, index)
    if loc[0] == "split":
        return "location.index " + str(index) + " falls inside a UTF-16 surrogate pair."
    els = model[loc[1]]["elements"]
    ei = loc[2]
    if ei > len(els):
        ei = len(els)
    els.insert(ei, el)
    _replace_model(model, _normalize(model))
    return None

# _apply_delete_range deletes the UTF-16 units in [start, end). Deleting a
# paragraph's newline merges it with the following paragraph. The document's
# final newline cannot be deleted (matching the real API).
def _apply_delete_range(model, start, end):
    total = _u16_len(_model_text(model))
    if start < 1 or end <= start or end > total:
        return "range startIndex=" + str(start) + ", endIndex=" + str(end) + " is invalid; the document's final newline cannot be deleted."
    out = []
    cur = None
    pos = 1
    for para in model:
        plen = _u16_len(_para_vtext(para))
        ps = pos
        pe = pos + plen
        if cur == None:
            cur = _new_para(_copy_style(para["style"]), para["bullet"])
        ep = ps
        for el in para["elements"]:
            elen = _el_u16_len(el)
            es = ep
            ee = ep + elen
            ep = ee
            if ee <= start or es >= end:
                cur["elements"].append(_copy_el(el))
                continue
            if el["t"] == "text":
                a = start if start > es else es
                b = end if end < ee else ee
                ra = _u16_off(el["text"], a - es)
                rb = _u16_off(el["text"], b - es)
                if ra[1] or rb[1]:
                    return "range boundary falls inside a UTF-16 surrogate pair."
                if ra[0] > 0:
                    cur["elements"].append(_new_text_el(el["text"][:ra[0]], _copy_style(el["style"])))
                if rb[0] < len(el["text"]):
                    cur["elements"].append(_new_text_el(el["text"][rb[0]:], _copy_style(el["style"])))
            # page breaks / inline objects inside the range are dropped
        if not (start <= pe - 1 and pe - 1 < end):
            out.append(cur)
            cur = None
        pos = pe
    if cur != None:
        out.append(cur)
    _replace_model(model, _normalize(out))
    return None

# _validate_range extracts the [startIndex, endIndex) pair for style/bullet
# requests. Returns [start, end].
def _validate_range(rng):
    if rng == None:
        return [-1, -1]
    start = _num_to_int(rng.get("startIndex"), -1)
    end = _num_to_int(rng.get("endIndex"), -1)
    return [start, end]

# _parse_fields turns a docs field mask ("bold,italic" or "*") into the list
# of keys to copy from the style payload. Absent mask = every provided key.
def _parse_fields(fields, payload):
    if payload == None or type(payload) != "dict":
        return []
    if fields == None or fields == "*" or fields == "":
        out = []
        for k in payload:
            out.append(k)
        return out
    cleaned = str(fields).replace(" ", "")
    parts = cleaned.split(",")
    out = []
    for p in parts:
        if p != "":
            out.append(p)
    return out

# _apply_style_to_field writes one field into a style dict, clearing it when
# the payload omits the field (docs field-mask semantics).
def _apply_style_to_field(style, payload, f):
    if f in payload:
        style[f] = payload[f]
    elif f in style:
        style.pop(f)

_NAMED_STYLES = [
    "NORMAL_TEXT",
    "HEADING_1",
    "HEADING_2",
    "HEADING_3",
    "HEADING_4",
    "HEADING_5",
    "HEADING_6",
    "TITLE",
    "SUBTITLE",
]

# _apply_update_paragraph_style applies paragraphStyle fields (honoring the
# fields mask) to every paragraph intersecting the range.
def _apply_update_paragraph_style(model, spec):
    r = _validate_range(spec.get("range"))
    start = r[0]
    end = r[1]
    total = _u16_len(_model_text(model))
    if start < 1 or end <= start or end > total + 1:
        return "range startIndex=" + str(start) + ", endIndex=" + str(end) + " is invalid."
    ps_style = spec.get("paragraphStyle")
    if ps_style == None:
        ps_style = {}
    fields = _parse_fields(spec.get("fields"), ps_style)
    if len(fields) == 0:
        return "fields: at least one paragraphStyle field must be set."
    if "namedStyle" in fields:
        ns = ps_style.get("namedStyle")
        if ns == None or type(ns) != "string" or _is_named_style(ns) == False:
            return "paragraphStyle.namedStyle: " + str(ns) + " is not a valid named style."
    pos = 1
    for para in model:
        plen = _u16_len(_para_vtext(para))
        ps = pos
        pe = pos + plen
        if ps < end and pe > start:
            for f in fields:
                _apply_style_to_field(para["style"], ps_style, f)
        pos = pe
    return None

def _is_named_style(ns):
    for s in _NAMED_STYLES:
        if s == ns:
            return True
    return False

# _split_para_at splits a paragraph's text elements at the range boundaries
# (like the real API splitting runs when a style range cuts mid-run) so the
# style applies to exactly the covered units. Returns None or an error.
def _split_para_at(para, ps, start, end):
    changed = True
    while changed:
        changed = False
        ep = ps
        els = para["elements"]
        for k in range(len(els)):
            el = els[k]
            elen = _el_u16_len(el)
            es = ep
            ee = ep + elen
            ep = ee
            if el["t"] != "text" or ee <= start or es >= end:
                continue
            for b in [start, end]:
                if b > es and b < ee:
                    off = _u16_off(el["text"], b - es)
                    if off[1]:
                        return "range boundary falls inside a UTF-16 surrogate pair."
                    if off[0] > 0 and off[0] < len(el["text"]):
                        tail = el["text"][off[0]:]
                        el["text"] = el["text"][:off[0]]
                        els.insert(k + 1, _new_text_el(tail, _copy_style(el["style"])))
                        changed = True
                        break
            if changed:
                break
    return None

# _apply_update_text_style applies textStyle fields (honoring the fields
# mask) to every text element intersecting the range, splitting runs at the
# range boundaries first. A paragraph's trailing newline is never styled.
def _apply_update_text_style(model, spec):
    r = _validate_range(spec.get("range"))
    start = r[0]
    end = r[1]
    total = _u16_len(_model_text(model))
    if start < 1 or end <= start or end > total + 1:
        return "range startIndex=" + str(start) + ", endIndex=" + str(end) + " is invalid."
    ts = spec.get("textStyle")
    if ts == None:
        ts = {}
    fields = _parse_fields(spec.get("fields"), ts)
    if len(fields) == 0:
        return "fields: at least one textStyle field must be set."
    pos = 1
    for para in model:
        plen = _u16_len(_para_vtext(para))
        ps = pos
        pe = pos + plen
        if ps < end and pe > start:
            msg = _split_para_at(para, ps, start, end)
            if msg != None:
                return msg
        pos = pe
    pos = 1
    for para in model:
        plen = _u16_len(_para_vtext(para))
        ps = pos
        pe = pos + plen
        if ps < end and pe > start:
            ep = ps
            for el in para["elements"]:
                elen = _el_u16_len(el)
                es = ep
                ee = ep + elen
                ep = ee
                if es < end and ee > start and el["t"] == "text":
                    for f in fields:
                        _apply_style_to_field(el["style"], ts, f)
        pos = pe
    return None

# _glyph_for_preset maps a bulletPreset enum to its level-0 glyph (the first
# nesting level named in the preset).
def _glyph_for_preset(preset):
    p = str(preset)
    if p.find("DECIMAL") >= 0:
        return "1."
    if p.find("ALPHA") >= 0 or p.find("LETTER") >= 0:
        return "a."
    if p.find("ROMAN") >= 0:
        return "i."
    if p.startswith("NUMBERED"):
        return "1."
    return "•"

# _apply_create_paragraph_bullets bullets every paragraph intersecting the
# range. All paragraphs in one call join a single new list. Returns
# [err, list_entry] — the caller registers the list on the document.
def _apply_create_paragraph_bullets(model, spec):
    r = _validate_range(spec.get("range"))
    start = r[0]
    end = r[1]
    total = _u16_len(_model_text(model))
    if start < 1 or end <= start or end > total + 1:
        return ["range startIndex=" + str(start) + ", endIndex=" + str(end) + " is invalid.", None]
    preset = spec.get("bulletPreset")
    if preset == None:
        preset = "BULLET_DISC_CIRCLE_SQUARE"
    list_id = "list." + _gen_token(store_kv_incr("gdocs", "list_seq"))
    glyph = _glyph_for_preset(preset)
    pos = 1
    for para in model:
        plen = _u16_len(_para_vtext(para))
        ps = pos
        pe = pos + plen
        if ps < end and pe > start:
            para["bullet"] = {"listId": list_id, "nestingLevel": 0}
        pos = pe
    entry = {
        "listProperties": {
            "nestingLevels": [
                {
                    "glyph": glyph,
                    "indentFirstLine": {"magnitude": 18, "unit": "PT"},
                    "indentStart": {"magnitude": 36, "unit": "PT"},
                },
            ],
        },
    }
    return [None, [list_id, entry]]

# _apply_delete_paragraph_bullets removes the bullet from every paragraph
# intersecting the range.
def _apply_delete_paragraph_bullets(model, spec):
    r = _validate_range(spec.get("range"))
    start = r[0]
    end = r[1]
    total = _u16_len(_model_text(model))
    if start < 1 or end <= start or end > total + 1:
        return "range startIndex=" + str(start) + ", endIndex=" + str(end) + " is invalid."
    pos = 1
    for para in model:
        plen = _u16_len(_para_vtext(para))
        ps = pos
        pe = pos + plen
        if ps < end and pe > start:
            para["bullet"] = None
        pos = pe
    return None

# --- rendering -------------------------------------------------------------

def _render_pstyle(para):
    st = para["style"]
    out = {"namedStyle": st.get("namedStyle", "NORMAL_TEXT")}
    for k in [
        "alignment",
        "direction",
        "indentEnd",
        "indentFirstLine",
        "indentStart",
        "spaceAbove",
        "spaceBelow",
        "spacingMode",
        "borderBetween",
        "avoidWidowAndOrphan",
        "shading",
    ]:
        if k in st:
            out[k] = st[k]
    if para["bullet"] != None and "indentFirstLine" not in out:
        out["indentFirstLine"] = {"magnitude": 18, "unit": "PT"}
        out["indentStart"] = {"magnitude": 36, "unit": "PT"}
    return out

# _render_content derives the ranged body.content array from the model:
# one content item per paragraph with startIndex/endIndex in UTF-16 units,
# and per-element spans. The paragraph's trailing newline is rendered inside
# the last textRun when possible (as the real API does).
def _render_content(model):
    content = []
    pos = 1
    for para in model:
        vtext = _para_vtext(para)
        end = pos + _u16_len(vtext)
        els = para["elements"]
        n = len(els)
        rendered = []
        ep = pos
        for i in range(n):
            el = els[i]
            if el["t"] == "text":
                txt = el["text"]
                if i == n - 1:
                    txt = txt + "\n"
                elen = _u16_len(txt)
                run = {"content": txt}
                if len(el["style"]) > 0:
                    run["textStyle"] = _copy_style(el["style"])
                rendered.append({"startIndex": ep, "endIndex": ep + elen, "textRun": run})
                ep += elen
            elif el["t"] == "brk":
                rendered.append({"startIndex": ep, "endIndex": ep + 1, "pageBreak": {}})
                ep += 1
            else:
                rendered.append({
                    "startIndex": ep,
                    "endIndex": ep + 1,
                    "inlineObjectElement": {
                        "inlineObjectId": el["objectId"],
                        "textStyle": {},
                    },
                })
                ep += 1
        if n == 0 or els[n - 1]["t"] != "text":
            rendered.append({"startIndex": ep, "endIndex": ep + 1, "textRun": {"content": "\n"}})
            ep += 1
        pd = {"elements": rendered, "paragraphStyle": _render_pstyle(para)}
        if para["bullet"] != None:
            b = _copy_style(para["bullet"])
            b["textStyle"] = {}
            pd["bullet"] = b
        content.append({"startIndex": pos, "endIndex": end, "paragraph": pd})
        pos = end
    return content

# --- ids / seeding ---------------------------------------------------------

_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

def _gen_token(n):
    base = ""
    val = n * (7900 + 19) + (104*1000 + 729)
    for i in range(40):
        base = base + _B64URL[val % 64]
        val = val // 64 + 31
    return base[:24]

def _gen_doc_id(n):
    return _gen_token(n)

# _seed creates a default document so GET works without prior POST.
def _seed():
    if store_kv_get("gdocs", "seeded") == "yes":
        return
    store_kv_set("gdocs", "seeded", "yes")

    doc_id = _gen_doc_id(0)
    store_kv_set("gdocs", "default_doc_id", doc_id)

    dc = store_collection("documents")
    seed_doc = _build_doc(doc_id, "Untitled document", [])
    _touch_doc(seed_doc)
    dc.insert(seed_doc)

# _build_doc constructs a document record carrying the internal model plus
# its inline-object and list registries.
def _build_doc(doc_id, title, model):
    if len(model) == 0:
        model = [_new_para(None, None)]
    return {
        "id": doc_id,
        "documentId": doc_id,
        "title": title,
        "model": model,
        "inlineObjects": {},
        "lists": {},
    }

# _find_doc looks up a document by documentId.
def _find_doc(doc_id):
    dc = store_collection("documents")
    for doc in dc.list():
        if doc.get("documentId") == doc_id:
            return doc
    return None

# _doc_model returns the document's model, deriving one from a legacy
# plain-body record if needed.
def _doc_model(doc):
    model = doc.get("model")
    if model != None and type(model) == "list":
        return model
    body = doc.get("body")
    if body == None:
        return [_new_para(None, None)]
    text = _get_content_text(body.get("content", []))
    return _model_from_text(text)

# _model_from_text builds a paragraph model from a plain text string.
def _model_from_text(text):
    model = []
    parts = text.split("\n")
    for i in range(len(parts) - 1):
        para = _new_para(None, None)
        if parts[i] != "":
            para["elements"].append(_new_text_el(parts[i], {}))
        model.append(para)
    return model

# _get_content_text extracts the full text from a rendered body content
# array (legacy compatibility).
def _get_content_text(content):
    text = ""
    for item in content:
        para = item.get("paragraph", {})
        elements = para.get("elements", [])
        for elem in elements:
            text_run = elem.get("textRun", {})
            text = text + text_run.get("content", "")
    return text

# --- inline images ---------------------------------------------------------

_B64_CHARS = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=-_"

# _clean_b64 strips whitespace from base64 payloads.
def _clean_b64(data):
    return str(data).replace("\n", "").replace("\r", "").replace(" ", "").replace("\t", "")

# _valid_b64 checks that a cleaned payload is well-formed base64 (standard
# or URL-safe alphabet, multiple of 4).
def _valid_b64(data):
    if len(data) == 0 or len(data) % 4 != 0:
        return False
    # '=' is padding and only legal as the last <=2 chars; anywhere else the
    # stdlib decoder raises (surfacing as a 500 instead of a 400).
    stripped = 0
    while stripped < 2 and len(data) - stripped > 0 and data[len(data) - stripped - 1] == "=":
        stripped = stripped + 1
    core = data[:len(data) - stripped]
    if "=" in core:
        return False
    for i in range(len(core)):
        if core[i] not in _B64_CHARS:
            return False
    return True

# _decode_b64 decodes standard or URL-safe base64. Returns [bytes, None] on
# success or [None, message] on a bad payload (checked beforehand so the
# crypto builtins never turn a bad request into a 500).
def _decode_b64(data):
    cleaned = _clean_b64(data)
    if not _valid_b64(cleaned):
        return [None, "Invalid base64 image data."]
    for i in range(len(cleaned)):
        c = cleaned[i]
        if c == "-" or c == "_" or c == "+" or c == "/":
            if c == "-" or c == "_":
                return [crypto.base64url_decode(cleaned), None]
            return [crypto.base64_decode(cleaned), None]
    return [crypto.base64_decode(cleaned), None]

# _guess_image_mime guesses a MIME type from a URI extension.
def _guess_image_mime(uri):
    u = str(uri).lower()
    if u.endswith(".jpg") or u.endswith(".jpeg"):
        return "image/jpeg"
    if u.endswith(".gif"):
        return "image/gif"
    if u.endswith(".webp"):
        return "image/webp"
    if u.endswith(".svg"):
        return "image/svg+xml"
    return "image/png"
