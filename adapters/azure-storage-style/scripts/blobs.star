# Blob handlers for Azure Blob Storage — stateful PUT/GET/HEAD/DELETE +
# ListBlobs XML.
#
# PUT   /{container}/{blob}                    -> upload BlockBlob (201)
# GET   /{container}/{blob}                    -> download content (200)
# HEAD  /{container}/{blob}                    -> metadata headers (200)
# DELETE /{container}/{blob}                   -> delete blob (202)
# GET   /{container}?restype=container&comp=list -> ListBlobs XML (200)
# PUT   /{container}/{blob}?comp=properties    -> set blob properties (200)
# GET   /{container}/{blob}?comp=metadata      -> get blob metadata (200)
# PUT   /{container}/{blob}?comp=metadata      -> set blob metadata (200)
#
# Blobs are STATEFUL: an uploaded blob appears in ListBlobs for the same
# container, enabling round-trip testing.
#
# Shared helpers (_require_auth, _xml_*, _gen_etag) are preloaded from
# scripts/lib.star.

# _has_query_key returns True if the given query key exists.
def _has_query_key(req, key):
    query = req.get("query")
    if query == None:
        return False
    for k in query:
        if k == key:
            return True
    return False

# _query_val returns the query value for a key, or "".
def _query_val(req, key):
    query = req.get("query")
    if query == None:
        return ""
    val = query.get(key, "")
    if val == None:
        return ""
    return val

# on_container_get handles GET /{container} — dispatches between ListBlobs
# and other container GET operations based on query params.
def on_container_get(req):
    err = _require_auth(req)
    if err != None:
        return err

    # ListBlobs: ?restype=container&comp=list
    if _has_query_key(req, "comp"):
        comp = _query_val(req, "comp")
        if comp == "list":
            return _list_blobs(req)
    # Default: treat as list if restype=container
    if _has_query_key(req, "restype"):
        return _list_blobs(req)
    # Otherwise list blobs
    return _list_blobs(req)

# _list_blobs returns the ListBlobs XML response.
def _list_blobs(req):
    container = req["params"]["container"]

    # Check container exists
    cc = store_collection("containers")
    container_exists = False
    for c in cc.list():
        if c.get("name", "") == container:
            container_exists = True
            break
    if not container_exists:
        return _container_not_found(container)

    bc = store_collection("blobs")
    prefix = _query_val(req, "prefix")

    matching = []
    for b in bc.list():
        if b.get("container", "") != container:
            continue
        name = b.get("name", "")
        if prefix != "" and not _has_prefix(name, prefix):
            continue
        matching.append(b)

    # Apply paging (maxresults + marker) after prefix filtering.
    matching, next_marker = _list_page(req, matching)
    if matching == None:
        return _az_error(400, "InvalidQueryParameterValue", "Value for one of the query parameters specified in the request URI is invalid.")

    xml = '<?xml version="1.0" encoding="utf-8"?>\n'
    xml = xml + '<EnumerationResults ServiceEndpoint="http://stunt.local/" ContainerName="' + _xml_escape(container) + '">\n'
    xml = xml + "  <Blobs>\n"
    for b in matching:
        name = b.get("name", "")
        blob_type = b.get("blobType", "BlockBlob")
        content_length = b.get("contentLength", 0)
        etag = b.get("etag", "")
        last_modified = b.get("lastModified", _rfc1123())
        content_type = b.get("contentType", "application/octet-stream")

        xml = xml + "    <Blob>\n"
        xml = xml + "      <Name>" + _xml_escape(name) + "</Name>\n"
        xml = xml + "      <Properties>\n"
        xml = xml + "        <BlobType>" + _xml_escape(blob_type) + "</BlobType>\n"
        xml = xml + "        <ContentType>" + _xml_escape(content_type) + "</ContentType>\n"
        xml = xml + "        <ContentLength>" + _to_int_str(content_length) + "</ContentLength>\n"
        xml = xml + "        <LastModified>" + _xml_escape(last_modified) + "</LastModified>\n"
        xml = xml + "        <Etag>" + _xml_escape(etag) + "</Etag>\n"
        xml = xml + "      </Properties>\n"
        xml = xml + "    </Blob>\n"
    xml = xml + "  </Blobs>\n"
    if next_marker == "":
        xml = xml + "  <NextMarker />\n"
    else:
        xml = xml + "  <NextMarker>" + _xml_escape(next_marker) + "</NextMarker>\n"
    xml = xml + "</EnumerationResults>"
    return respond(200, xml, {"Content-Type": "application/xml", "x-ms-request-id": _req_id()})

# on_put_blob handles blob upload or blob sub-operations (?comp=block,
# ?comp=blocklist, ?comp=properties, ?comp=metadata).
def on_put_blob(req):
    err = _require_auth(req)
    if err != None:
        return err

    container = req["params"]["container"]
    blob = req["params"]["blob"]

    # Dispatch on comp query param
    if _has_query_key(req, "comp"):
        comp = _query_val(req, "comp")
        if comp == "properties":
            return _set_blob_properties(req, container, blob)
        if comp == "metadata":
            return _set_blob_metadata(req, container, blob)
        if comp == "block":
            return _put_block(req, container, blob)
        if comp == "blocklist":
            return _put_block_list(req, container, blob)
        # Other comp values — accept generically
        return respond(201, "", {"x-ms-request-id": _req_id()})

    # Regular blob upload
    return _upload_blob(req, container, blob)

# _upload_blob stores a BlockBlob.
def _upload_blob(req, container, blob):
    # Check that container exists
    cc = store_collection("containers")
    container_exists = False
    for c in cc.list():
        if c.get("name", "") == container:
            container_exists = True
            break
    if not container_exists:
        return _container_not_found(container)

    headers = req.get("headers")
    if headers == None:
        headers = {}

    blob_type = headers.get("x-ms-blob-type", "BlockBlob")
    if blob_type == None:
        blob_type = "BlockBlob"
    content_type = headers.get("Content-Type", "application/octet-stream")
    if content_type == None:
        content_type = "application/octet-stream"

    # Byte-exact content in the blob store; raw_body is the verbatim request
    # bytes (a parsed body map cannot represent binary content).
    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    content_length = len(raw)
    etag = _gen_etag()

    # Collect x-ms-meta-* metadata headers
    metadata = {}
    for k in headers:
        if _has_prefix(k.lower(), "x-ms-meta-"):
            metadata[k] = headers[k]

    bc = store_collection("blobs")
    # Reuse the existing blob id on overwrite; otherwise mint a path-safe one.
    bid = None
    existing_id = None
    for b in bc.list():
        if b.get("container", "") == container and b.get("name", "") == blob:
            bid = b.get("bid", "")
            existing_id = b.get("id", "")
            break
    if bid == None or bid == "":
        bid = "azb_" + str(store_kv_incr("azure", "blob_seq"))
    store_blob("az-blobs").put(bid, raw, content_type)

    doc = {
        "container": container,
        "name": blob,
        "bid": bid,
        "contentType": content_type,
        "blobType": blob_type,
        "contentLength": content_length,
        "etag": etag,
        "lastModified": _rfc1123(),
        "creationTime": _creation_time(),
        "metadata": metadata,
        # A single-shot Put Blob has no addressable committed blocks; only
        # Put Block List commits named blocks (see _put_block_list).
        "committedBlocks": [],
    }
    if existing_id != None and existing_id != "":
        bc.update(existing_id, doc)
    else:
        bc.insert(doc)

    return respond(201, "", {
        "ETag": '"' + etag + '"',
        "Last-Modified": _rfc1123(),
        "x-ms-request-id": _req_id(),
        "x-ms-version": "2024-08-04",
        "Content-MD5": "",
    })

# on_get_blob returns the blob content or metadata.
def on_get_blob(req):
    err = _require_auth(req)
    if err != None:
        return err

    container = req["params"]["container"]
    blob = req["params"]["blob"]

    # ?comp=metadata -> return blob metadata
    if _has_query_key(req, "comp"):
        comp = _query_val(req, "comp")
        if comp == "metadata":
            return _get_blob_metadata(req, container, blob)
        if comp == "blocklist":
            return _get_block_list(req, container, blob)
        if comp == "properties":
            return _get_blob_properties(req, container, blob)

    # Regular download
    bc = store_collection("blobs")
    b = None
    for blk in bc.list():
        if blk.get("container", "") == container and blk.get("name", "") == blob:
            b = blk
            break
    if b == None:
        return _blob_not_found(container, blob)

    content = store_blob("az-blobs").get(b.get("bid", ""))
    if content == None:
        content = ""
    content_type = b.get("contentType", "application/octet-stream")
    if content_type == None:
        content_type = "application/octet-stream"

    return respond(200, content, {
        "Content-Type": content_type,
        "Content-Length": str(len(content)),
        "ETag": '"' + b.get("etag", "") + '"',
        "Last-Modified": b.get("lastModified", _rfc1123()),
        "x-ms-blob-type": b.get("blobType", "BlockBlob"),
        "x-ms-request-id": _req_id(),
    })

# on_head_blob returns blob metadata headers only.
def on_head_blob(req):
    err = _require_auth(req)
    if err != None:
        return err

    container = req["params"]["container"]
    blob = req["params"]["blob"]

    bc = store_collection("blobs")
    b = None
    for blk in bc.list():
        if blk.get("container", "") == container and blk.get("name", "") == blob:
            b = blk
            break
    if b == None:
        return _blob_not_found(container, blob)

    return respond(200, "", {
        "Content-Length": str(_to_int_str(b.get("contentLength", 0))),
        "Content-Type": b.get("contentType", "application/octet-stream"),
        "ETag": '"' + b.get("etag", "") + '"',
        "Last-Modified": b.get("lastModified", _rfc1123()),
        "x-ms-blob-type": b.get("blobType", "BlockBlob"),
        "x-ms-creation-time": b.get("creationTime", _creation_time()),
        "x-ms-request-id": _req_id(),
    })

# on_delete_blob deletes a blob (and any staged uncommitted blocks, like the
# real service). Returns 202.
def on_delete_blob(req):
    err = _require_auth(req)
    if err != None:
        return err

    container = req["params"]["container"]
    blob = req["params"]["blob"]

    bc = store_collection("blobs")
    b_id = None
    bid = None
    for blk in bc.list():
        if blk.get("container", "") == container and blk.get("name", "") == blob:
            b_id = blk.get("id", "")
            bid = blk.get("bid", "")
            break
    if b_id != None and b_id != "":
        bc.delete(b_id)
    if bid != None and bid != "":
        store_blob("az-blobs").delete(bid)
    _discard_staged_blocks(container, blob)

    return respond(202, "", {"x-ms-request-id": _req_id()})

# ====================================================================
# Blob sub-operations (?comp=...)
# ====================================================================

# _set_blob_properties: PUT /{container}/{blob}?comp=properties
def _set_blob_properties(req, container, blob):
    bc = store_collection("blobs")
    b_id = None
    for blk in bc.list():
        if blk.get("container", "") == container and blk.get("name", "") == blob:
            b_id = blk.get("id", "")
            break
    if b_id == None or b_id == "":
        return _blob_not_found(container, blob)

    headers = req.get("headers")
    if headers != None:
        # Update content-type if provided
        ct = headers.get("x-ms-blob-content-type", None)
        if ct != None:
            b = bc.get(b_id)
            if b != None:
                b["contentType"] = ct
                bc.update(b_id, b)
    return respond(200, "", {"x-ms-request-id": _req_id()})

# _set_blob_metadata: PUT /{container}/{blob}?comp=metadata
def _set_blob_metadata(req, container, blob):
    bc = store_collection("blobs")
    b_id = None
    for blk in bc.list():
        if blk.get("container", "") == container and blk.get("name", "") == blob:
            b_id = blk.get("id", "")
            break
    if b_id == None or b_id == "":
        return _blob_not_found(container, blob)

    headers = req.get("headers")
    metadata = {}
    if headers != None:
        for k in headers:
            if _has_prefix(k.lower(), "x-ms-meta-"):
                metadata[k] = headers[k]
    b = bc.get(b_id)
    if b != None:
        b["metadata"] = metadata
        bc.update(b_id, b)
    return respond(200, "", {"x-ms-request-id": _req_id()})

# _get_blob_metadata: GET /{container}/{blob}?comp=metadata
def _get_blob_metadata(req, container, blob):
    bc = store_collection("blobs")
    b = None
    for blk in bc.list():
        if blk.get("container", "") == container and blk.get("name", "") == blob:
            b = blk
            break
    if b == None:
        return _blob_not_found(container, blob)

    resp_headers = {"x-ms-request-id": _req_id()}
    metadata = b.get("metadata", None)
    if metadata != None:
        for k in metadata:
            resp_headers[k] = metadata[k]
    return respond(200, "", resp_headers)

# _get_blob_properties: GET /{container}/{blob}?comp=properties
def _get_blob_properties(req, container, blob):
    bc = store_collection("blobs")
    b = None
    for blk in bc.list():
        if blk.get("container", "") == container and blk.get("name", "") == blob:
            b = blk
            break
    if b == None:
        return _blob_not_found(container, blob)

    return respond(200, "", {
        "x-ms-blob-type": b.get("blobType", "BlockBlob"),
        "Content-Length": _to_int_str(b.get("contentLength", 0)),
        "Content-Type": b.get("contentType", "application/octet-stream"),
        "x-ms-request-id": _req_id(),
    })

# ====================================================================
# Block staging (Put Block / Put Block List / Get Block List)
# ====================================================================
# The real Azure block-blob model: Put Block stages bytes under a
# base64 block id (out of order, re-uploadable), Put Block List commits
# the blob by concatenating the listed blocks in order, and Get Block
# List enumerates committed vs uncommitted blocks. Committed content is
# byte-exact: GET /{container}/{blob} returns the assembled bytes.
#
# Documented deviation: the crypto module has no MD5, so Content-MD5
# response headers carry the base64 SHA-256 of the bytes instead (the S3
# adapter makes the same trade for its ETags).

# _put_block: PUT /{container}/{blob}?comp=block&blockid=<base64>
# Stages the request body as an uncommitted block. Blocks may be staged in
# any order; re-staging an id replaces its bytes.
def _put_block(req, container, blob):
    if not _container_exists(container):
        return _container_not_found(container)

    block_id = _query_val(req, "blockid")
    if block_id == "":
        return _az_blob_error(400, "MissingRequiredQueryParameter",
            "A query parameter that's mandatory for this request is not specified.\nRequestId:" + _req_id() + "\nTime:" + _rfc1123())
    err = _validate_block_id(block_id)
    if err != None:
        return err

    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    digest = crypto.sha256(raw, "base64")

    bstore = store_blob("az-blocks")
    bc = store_collection("blocks")
    row_id = _block_row_id(container, blob, block_id)
    existing = bc.get(row_id)
    bid = ""
    if existing != None:
        bid = existing.get("bid", "")
    if bid == None or bid == "":
        bid = "azblk_" + str(store_kv_incr("azure", "block_seq"))
    bstore.put(bid, raw, "application/octet-stream")

    doc = {
        "id": row_id,
        "container": container,
        "blob": blob,
        "blockId": block_id,
        "bid": bid,
        "size": len(raw),
        "sha256b64": digest,
        "lastModifiedUnix": clock.now_unix(),
    }
    if existing == None:
        bc.insert(doc)
    else:
        bc.update(row_id, doc)

    return respond(201, "", {
        "Content-MD5": digest,
        "x-ms-request-id": _req_id(),
        "x-ms-version": "2024-08-04",
    })

# _invalid_block_list returns the real Azure 400 InvalidBlockList error.
def _invalid_block_list():
    return _az_blob_error(400, "InvalidBlockList",
        "The specified block list is invalid.\nRequestId:" + _req_id() + "\nTime:" + _rfc1123())

# _put_block_list: PUT /{container}/{blob}?comp=blocklist
# Commits the blob: the listed blocks (in list order) are concatenated into
# the blob content, unlisted staged blocks are discarded, and the listed
# blocks become the blob's committed block list.
def _put_block_list(req, container, blob):
    if not _container_exists(container):
        return _container_not_found(container)

    raw = req.get("raw_body", "")
    if raw == None:
        raw = ""
    entries = _parse_block_list_xml(raw)
    if entries == None:
        return _az_blob_error(400, "InvalidXMLDocument",
            "XML specified is not syntactically valid.\nRequestId:" + _req_id() + "\nTime:" + _rfc1123())

    bdoc = _find_blob(container, blob)
    committed = []
    if bdoc != None:
        cb = bdoc.get("committedBlocks", None)
        if cb != None:
            committed = cb

    # Resolve each listed id to staged bytes (Latest/Uncommitted) or an
    # already-committed block (Committed); anything missing is a 400.
    bstore = store_blob("az-blocks")
    staged = {}
    for r in _staged_blocks(container, blob):
        staged[r.get("blockId", "")] = r
    committed_map = {}
    for c in committed:
        committed_map[c.get("Name", "")] = c

    full = ""
    committed_list = []
    for entry in entries:
        kind = entry[0]
        block_id = entry[1]
        if block_id == "" or not _is_base64(block_id) or len(block_id) > _MAX_BLOCK_ID_CHARS:
            return _invalid_block_list()
        row = staged.get(block_id, None)
        if row != None:
            content = bstore.get(row.get("bid", ""))
            if content == None:
                content = ""
            full = full + content
            committed_list.append({"Name": block_id, "Size": _to_num(row.get("size", 0))})
            continue
        if kind == "Committed":
            cb = committed_map.get(block_id, None)
            if cb != None:
                # Reuse the committed bytes from the assembled blob content:
                # slice the previously committed range out of the doc.
                piece = _committed_bytes(bdoc, cb)
                if piece != None:
                    full = full + piece
                    committed_list.append({"Name": block_id, "Size": _to_num(cb.get("Size", 0))})
                    continue
        return _invalid_block_list()

    # Assemble: reuse the existing blob's backing id when overwriting.
    bc = store_collection("blobs")
    bid = ""
    existing_id = ""
    if bdoc != None:
        bid = bdoc.get("bid", "")
        existing_id = bdoc.get("id", "")
    if bid == None or bid == "":
        bid = "azb_" + str(store_kv_incr("azure", "blob_seq"))
    content_type = "application/octet-stream"
    if bdoc != None:
        ct = bdoc.get("contentType", "")
        if ct != None and ct != "":
            content_type = ct
    store_blob("az-blobs").put(bid, full, content_type)

    metadata = {}
    if bdoc != None:
        md = bdoc.get("metadata", None)
        if md != None:
            metadata = md
    doc = {
        "container": container,
        "name": blob,
        "bid": bid,
        "contentType": content_type,
        "blobType": "BlockBlob",
        "contentLength": len(full),
        "etag": _gen_etag(),
        "lastModified": _rfc1123(),
        "creationTime": _creation_time(),
        "metadata": metadata,
        "committedBlocks": committed_list,
    }
    if existing_id != None and existing_id != "":
        bc.update(existing_id, doc)
    else:
        bc.insert(doc)

    # Staged blocks are consumed by the commit; unlisted ones are discarded.
    _discard_staged_blocks(container, blob)

    return respond(201, "", {
        "ETag": '"' + doc["etag"] + '"',
        "Last-Modified": doc["lastModified"],
        "Content-MD5": crypto.sha256(full, "base64"),
        "x-ms-request-id": _req_id(),
        "x-ms-version": "2024-08-04",
    })

# _committed_bytes returns the byte range of a committed block by replaying
# the committed list offsets against the blob content (Put Block List with
# <Committed> entries reuses previously committed bytes). Returns None when
# the block is not part of the current committed content.
def _committed_bytes(bdoc, committed_block):
    if bdoc == None:
        return None
    content = store_blob("az-blobs").get(bdoc.get("bid", ""))
    if content == None:
        return None
    offset = 0
    want = committed_block.get("Name", "")
    for c in bdoc.get("committedBlocks", []):
        size = _to_num(c.get("Size", 0))
        if c.get("Name", "") == want:
            return content[offset:offset + size]
        offset = offset + size
    return None

# _get_block_list: GET /{container}/{blob}?comp=blocklist&blocklisttype=...
# Returns the Committed/Uncommitted block lists. blocklisttype is required
# (committed | uncommitted | all), like the real service.
def _get_block_list(req, container, blob):
    if not _container_exists(container):
        return _container_not_found(container)

    blt = _query_val(req, "blocklisttype")
    if blt == "":
        return _az_blob_error(400, "MissingRequiredQueryParameter",
            "A query parameter that's mandatory for this request is not specified.\nRequestId:" + _req_id() + "\nTime:" + _rfc1123())
    if blt != "committed" and blt != "uncommitted" and blt != "all":
        return _az_blob_error(400, "InvalidQueryParameterValue",
            "Value for one of the query parameters specified in the request URI is invalid.\nRequestId:" + _req_id() + "\nTime:" + _rfc1123())

    bdoc = _find_blob(container, blob)
    staged = _staged_blocks(container, blob)
    if bdoc == None and len(staged) == 0:
        return _blob_not_found(container, blob)

    xml = '<?xml version="1.0" encoding="utf-8"?>\n'
    xml = xml + "<BlockList>"
    if blt == "committed" or blt == "all":
        xml = xml + "<CommittedBlocks>"
        if bdoc != None:
            for c in bdoc.get("committedBlocks", []):
                xml = xml + "<Block>"
                xml = xml + "<Name>" + _xml_escape(c.get("Name", "")) + "</Name>"
                xml = xml + "<Size>" + _to_int_str(c.get("Size", 0)) + "</Size>"
                xml = xml + "</Block>"
        xml = xml + "</CommittedBlocks>"
    if blt == "uncommitted" or blt == "all":
        xml = xml + "<UncommittedBlocks>"
        for r in staged:
            xml = xml + "<Block>"
            xml = xml + "<Name>" + _xml_escape(r.get("blockId", "")) + "</Name>"
            xml = xml + "<Size>" + _to_int_str(r.get("size", 0)) + "</Size>"
            xml = xml + "</Block>"
        xml = xml + "</UncommittedBlocks>"
    xml = xml + "</BlockList>"
    return respond(200, xml, {"Content-Type": "application/xml", "x-ms-request-id": _req_id()})

# ====================================================================
# Helpers
# ====================================================================

# ====================================================================
# Error responses (Azure Storage XML shape)
# ====================================================================

def _blob_not_found(container, blob):
    xml = '<?xml version="1.0" encoding="utf-8"?>\n'
    xml = xml + "<Error><Code>BlobNotFound</Code>"
    xml = xml + "<Message>The specified blob does not exist.</Message>"
    xml = xml + "</Error>"
    return respond(404, xml, {"Content-Type": "application/xml", "x-ms-request-id": _req_id()})
