# Record transform handler — NetSuite SuiteTalk REST "!transform" operations.
#
# POST /services/rest/record/v1/{recordType}/{id}/!transform/{targetType}
#
# Supported chains (see _TRANSFORMS in lib.star):
#   salesOrder  -> invoice          (billing a sales order)
#   invoice     -> customerPayment  (applying a payment to an invoice)
#   opportunity -> salesOrder       (converting a won opportunity)
#
# The request body carries additional fields to set on the NEW record and
# overrides the mapped defaults (NetSuite semantics). Like the real service,
# a successful transform responds 204 No Content with the Location header of
# the new record. Transforming an invoice to a customerPayment also applies
# the payment and flips the source invoice to "Paid in Full" (NetSuite's
# applied-payment state).
#
# Errors (real NetSuite codes):
#   404 RCRD_DSNT_EXIST — source record does not exist
#   400 USER_ERROR      — source/target pair is not a transformable chain
#   400 INVALID_KEY_OR_REF — body reference that does not resolve
#   400 USER_ERROR      — mapped+overridden record missing a required field

# Shared helpers (_require_auth, _collection, _record_type_from_path,
# _get_body, _body_parse_error, _validate_create, _transform_doc,
# _TRAN_PREFIXES, _TRANSFORMS, _next_id, _netsuite_error) are preloaded from
# scripts/lib.star.

def on_transform(req):
    ok, err = _require_auth(req)
    if not ok:
        return err

    record_type = _record_type_from_path(req)
    allowed = _TRANSFORMS.get(record_type, [])
    target = req["params"].get("target", "")
    if target == None:
        target = ""

    # A target outside the record type's transform chain is USER_ERROR (the
    # real service refuses impossible transforms, e.g. customer -> invoice).
    if record_type == "" or target == "" or target not in allowed:
        return _netsuite_error(400,
            "An error occurred while updating records. Please try again.",
            "USER_ERROR",
            "You can not transform a record of type " + record_type + " to " + target + ".")

    col = _collection(record_type)
    if col == None:
        return _netsuite_error(404, "Not Found", "RCRD_TYPE_DSNT_EXIST",
            "Record type '" + record_type + "' does not exist.")

    record_id = req["params"].get("id", "")
    src = col.get(record_id)
    if src == None:
        return _netsuite_error(404, "Not Found", "RCRD_DSNT_EXIST",
            "That record does not exist.")

    body = _get_body(req)
    perr = _body_parse_error(req, body)
    if perr != None:
        return perr

    # Map the source record onto the target type, apply body overrides, and
    # validate the result with the same rules as a plain create.
    doc = _transform_doc(record_type, target, src, body)
    verr = _validate_create(target, doc)
    if verr != None:
        return verr

    new_id = _next_id(target)
    doc["id"] = new_id
    prefix = _TRAN_PREFIXES.get(target, "")
    if prefix != "":
        doc["tranId"] = prefix + new_id

    _collection(target).insert(doc)

    # Applying a customerPayment to its source invoice settles the invoice.
    if target == "customerPayment":
        upd = {}
        for k, v in src.items():
            upd[k] = v
        upd["status"] = "Paid in Full"
        col.update(record_id, upd)

    # NetSuite returns the new record's URL in the Location header (204).
    return respond(204, body=None, headers={
        "Location": "/services/rest/record/v1/" + target + "/" + new_id,
    })
