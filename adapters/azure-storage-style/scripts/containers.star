# Container + service-level handlers for Azure Blob Storage.
#
# GET   /?comp=list                -> ListContainers (XML)
# PUT   /{container}               -> Create container
# GET   /{container}?restype=container&comp=list -> List blobs (XML)
# HEAD  /{container}               -> container metadata
# DELETE /{container}              -> delete container
#
# Shared helpers (_require_auth, _xml_*, _gen_etag) are preloaded from
# scripts/lib.star.

# on_list_containers returns the ListContainers XML response.
# GET /?comp=list
def on_list_containers(req):
    err = _require_auth(req)
    if err != None:
        return err

    cc = store_collection("containers")
    containers = cc.list()

    xml = '<?xml version="1.0" encoding="utf-8"?>\n'
    xml = xml + '<EnumerationResults ServiceEndpoint="http://stunt.local/">\n'
    xml = xml + "  <Containers>\n"
    for c in containers:
        xml = xml + "    <Container>\n"
        xml = xml + "      <Name>" + _xml_escape(c.get("name", "")) + "</Name>\n"
        xml = xml + "      <Properties>\n"
        xml = xml + "        <Last-Modified>" + _xml_escape(c.get("lastModified", "Mon, 01 Jan 2024 00:00:00 GMT")) + "</Last-Modified>\n"
        xml = xml + "        <Etag>" + _xml_escape(c.get("etag", "")) + "</Etag>\n"
        pa = c.get("publicAccess", "")
        if pa == None:
            pa = ""
        if pa != "" and pa != "None":
            xml = xml + "        <PublicAccess>" + _xml_escape(pa) + "</PublicAccess>\n"
        xml = xml + "      </Properties>\n"
        xml = xml + "    </Container>\n"
    xml = xml + "  </Containers>\n"
    xml = xml + "  <NextMarker />\n"
    xml = xml + "</EnumerationResults>"
    return respond(200, xml, {"Content-Type": "application/xml", "x-ms-request-id": _req_id()})

# on_create_container creates a container.
# PUT /{container} (x-ms-blob-public-access)
def on_create_container(req):
    err = _require_auth(req)
    if err != None:
        return err

    container = req["params"]["container"]
    headers = req.get("headers")
    if headers == None:
        headers = {}
    public_access = headers.get("x-ms-blob-public-access", "")
    if public_access == None:
        public_access = ""

    etag = _gen_etag()

    cc = store_collection("containers")
    # Check if container already exists
    existing_id = None
    for c in cc.list():
        if c.get("name", "") == container:
            existing_id = c.get("id", "")
            break

    doc = {
        "name": container,
        "publicAccess": public_access,
        "lastModified": _rfc1123(),
        "etag": etag,
    }
    if existing_id != None and existing_id != "":
        cc.update(existing_id, doc)
    else:
        cc.insert(doc)

    resp_headers = {
        "ETag": '"' + etag + '"',
        "Last-Modified": _rfc1123(),
        "x-ms-request-id": _req_id(),
        "x-ms-version": "2024-08-04",
    }
    if public_access != "":
        resp_headers["x-ms-blob-public-access"] = public_access
    return respond(201, "", resp_headers)

# on_head_container returns container metadata headers.
# HEAD /{container}
def on_head_container(req):
    err = _require_auth(req)
    if err != None:
        return err

    container = req["params"]["container"]
    cc = store_collection("containers")
    c = None
    for cont in cc.list():
        if cont.get("name", "") == container:
            c = cont
            break
    if c == None:
        return _container_not_found(container)

    return respond(200, "", {
        "ETag": '"' + c.get("etag", "") + '"',
        "Last-Modified": c.get("lastModified", _rfc1123()),
        "x-ms-request-id": _req_id(),
        "x-ms-blob-public-access": c.get("publicAccess", "None"),
    })

# on_delete_container deletes a container.
# DELETE /{container}
def on_delete_container(req):
    err = _require_auth(req)
    if err != None:
        return err

    container = req["params"]["container"]
    cc = store_collection("containers")
    c_id = None
    for c in cc.list():
        if c.get("name", "") == container:
            c_id = c.get("id", "")
            break
    if c_id != None and c_id != "":
        cc.delete(c_id)
        # Also delete blobs in this container
        bc = store_collection("blobs")
        for b in bc.list():
            if b.get("container", "") == container:
                b_id = b.get("id", "")
                if b_id != None and b_id != "":
                    bc.delete(b_id)

    return respond(202, "", {"x-ms-request-id": _req_id()})

