# Shared library for gdocs-style adapter scripts.

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

# _to_int parses a decimal string to int.
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

# _gen_doc_id generates a realistic Google Docs document ID.
_B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

def _gen_doc_id(n):
    base = ""
    val = n * 7919 + 104729
    for i in range(40):
        base = base + _B64URL[val % 64]
        val = val // 64 + 31
    return base[:24]

# _seed creates a default document so GET works without prior POST.
def _seed():
    if store_kv_get("gdocs", "seeded") == "yes":
        return
    store_kv_set("gdocs", "seeded", "yes")

    doc_id = _gen_doc_id(0)
    store_kv_set("gdocs", "default_doc_id", doc_id)

    dc = store_collection("documents")
    dc.insert(_build_doc(doc_id, "Untitled document", []))

# _build_doc constructs a document object with the structural-editing model.
# content is a list of {startIndex, endIndex, paragraph:{elements:[...]}} items.
def _build_doc(doc_id, title, content):
    if len(content) == 0:
        content = [
            {
                "startIndex": 1,
                "endIndex": 2,
                "paragraph": {
                    "elements": [
                        {
                            "startIndex": 1,
                            "endIndex": 2,
                            "textRun": {"content": "\n"},
                        },
                    ],
                    "paragraphStyle": {},
                },
            },
        ]
    return {
        "id": doc_id,
        "documentId": doc_id,
        "title": title,
        "body": {"content": content},
    }

# _find_doc looks up a document by documentId.
def _find_doc(doc_id):
    dc = store_collection("documents")
    for doc in dc.list():
        if doc.get("documentId") == doc_id:
            return doc
    return None

# _get_content_text extracts the full text from a document's body content.
def _get_content_text(content):
    text = ""
    for item in content:
        para = item.get("paragraph", {})
        elements = para.get("elements", [])
        for elem in elements:
            text_run = elem.get("textRun", {})
            text = text + text_run.get("content", "")
    return text

# _rebuild_content reconstructs the body content array from a plain text string.
# This is the core of the structural-editing model: text is represented as
# ranged elements with startIndex/endIndex.
def _rebuild_content(text):
    if text == "":
        text = "\n"
    content = []
    # Split into paragraphs by newline, preserving them.
    pos = 1  # Google Docs uses 1-based indexing
    lines = text.split("\n")
    for i in range(len(lines)):
        line = lines[i]
        # Each line gets its own paragraph element.
        line_with_newline = line
        if i < len(lines) - 1:
            line_with_newline = line + "\n"
        end_idx = pos + len(line_with_newline)
        if line_with_newline == "":
            # Empty line (just a newline)
            content.append({
                "startIndex": pos,
                "endIndex": pos + 1,
                "paragraph": {
                    "elements": [
                        {
                            "startIndex": pos,
                            "endIndex": pos + 1,
                            "textRun": {"content": "\n"},
                        },
                    ],
                    "paragraphStyle": {},
                },
            })
            pos = pos + 1
        else:
            content.append({
                "startIndex": pos,
                "endIndex": end_idx,
                "paragraph": {
                    "elements": [
                        {
                            "startIndex": pos,
                            "endIndex": end_idx,
                            "textRun": {"content": line_with_newline},
                        },
                    ],
                    "paragraphStyle": {},
                },
            })
            pos = end_idx
    return content
