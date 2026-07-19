# Media upload handler.
#
# Faithful port of ***REMOVED***'s mock_x_api/server.py media surface:
#
#   POST /2/media/upload  (octet-stream body)  -> 200 { data:{ media_id_string } }
#
# The real /2/media/upload returns a top-level media_id_string, but this mock
# wraps it under `data` for consistency with the article endpoints (documented
# in the spec's module docstring). The TS adapter is built to match: it reads
# json.data.media_id_string.
#
# The uploaded blob bytes are not persisted — only the media_id is minted,
# since the mock's purpose is to validate the pipeline (the client gets a
# media_id to attach as cover_media_id on a subsequent draft).

def _pad5(n):
    if n < 10:
        return "0000" + str(n)
    if n < 100:
        return "000" + str(n)
    if n < 1000:
        return "00" + str(n)
    if n < 10000:
        return "0" + str(n)
    return str(n)

# on_upload accepts a media blob and returns a media_id_string.
def on_upload(req):
    media_id = "m_" + str(store_kv_incr("xarticles", "media_seq"))
    return respond(200, {"data": {"media_id_string": media_id}})
