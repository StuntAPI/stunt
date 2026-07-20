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
    xml = xml + "  <NextMarker />\n"
    xml = xml + "</EnumerationResults>"
    return respond(200, xml, {"Content-Type": "application/xml", "x-ms-request-id": _req_id()})

# on_put_blob handles blob upload or blob sub-operations (?comp=block,
# ?comp=properties, ?comp=metadata).
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
        # Other comp values (blocklist, etc.) — accept generically
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

    # Content from body (parsed JSON) or raw (nil body)
    body = req.get("body")
    if body == None:
        content_str = ""
    else:
        content_str = _body_to_str(body)

    content_length = len(content_str)
    etag = _gen_etag()

    # Collect x-ms-meta-* metadata headers
    metadata = {}
    for k in headers:
        if _has_prefix(k.lower(), "x-ms-meta-"):
            metadata[k] = headers[k]

    bc = store_collection("blobs")
    # Check if blob already exists -> update
    existing_id = None
    for b in bc.list():
        if b.get("container", "") == container and b.get("name", "") == blob:
            existing_id = b.get("id", "")
            break

    doc = {
        "container": container,
        "name": blob,
        "content": content_str,
        "contentType": content_type,
        "blobType": blob_type,
        "contentLength": content_length,
        "etag": etag,
        "lastModified": _rfc1123(),
        "creationTime": _creation_time(),
        "metadata": metadata,
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

    content = b.get("content", "")
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

# on_delete_blob deletes a blob. Returns 202.
def on_delete_blob(req):
    err = _require_auth(req)
    if err != None:
        return err

    container = req["params"]["container"]
    blob = req["params"]["blob"]

    bc = store_collection("blobs")
    b_id = None
    for blk in bc.list():
        if blk.get("container", "") == container and blk.get("name", "") == blob:
            b_id = blk.get("id", "")
            break
    if b_id != None and b_id != "":
        bc.delete(b_id)

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

# _put_block: PUT /{container}/{blob}?comp=block&blockid=...
def _put_block(req, container, blob):
    # Accept the block upload; return a Content-MD5
    return respond(201, "", {
        "Content-MD5": "",
        "x-ms-request-id": _req_id(),
    })

# _get_block_list: GET /{container}/{blob}?comp=blocklist
def _get_block_list(req, container, blob):
    xml = '<?xml version="1.0" encoding="utf-8"?>\n'
    xml = xml + '<BlockList><CommittedBlocks /><UncommittedBlocks /></BlockList>'
    return respond(200, xml, {"Content-Type": "application/xml", "x-ms-request-id": _req_id()})

# ====================================================================
# Helpers
# ====================================================================

# _body_to_str converts a parsed body map to a string representation.
def _body_to_str(body):
    if body == None:
        return ""
    return str(body)

# ====================================================================
# Error responses (Azure Storage XML shape)
# ====================================================================

def _blob_not_found(container, blob):
    xml = '<?xml version="1.0" encoding="utf-8"?>\n'
    xml = xml + "<Error><Code>BlobNotFound</Code>"
    xml = xml + "<Message>The specified blob does not exist.</Message>"
    xml = xml + "</Error>"
    return respond(404, xml, {"Content-Type": "application/xml", "x-ms-request-id": _req_id()})
