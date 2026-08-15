# Document handlers. Received (supplier / spend side) and issued (sales side)
# share the v2 CRUD shape; the generic machinery lives in lib.star.

_RECEIVED_DEFAULTS = {
    "type": "expense_receipt",
    "date": "",
    "description": "",
    "category": "",
    "amount_net": "0.00",
    "amount_vat": "0.00",
    "currency": "EUR",
    "entity": {"id": "", "name": ""},
    "items": [],
}

_ISSUED_DEFAULTS = {
    "type": "invoice",
    "date": "",
    "description": "",
    "category": "",
    "amount_net": "0.00",
    "amount_vat": "0.00",
    "currency": "EUR",
    "entity": {"id": "", "name": ""},
    "items": [],
}

def on_received_documents_list(req):
    return _crud_list(req, "received_documents")

def on_received_documents_create(req):
    return _crud_create(req, "received_documents", _RECEIVED_DEFAULTS)

def on_received_documents_info(req):
    return _categories_in_use(req, "received_documents")

def on_received_document_get(req):
    return _crud_get(req, "received_documents", "Document")

def on_received_document_modify(req):
    return _crud_modify(req, "received_documents", "Document")

def on_received_document_delete(req):
    return _crud_delete(req, "received_documents", "Document")

def on_issued_documents_list(req):
    return _crud_list(req, "issued_documents")

def on_issued_documents_create(req):
    return _crud_create(req, "issued_documents", _ISSUED_DEFAULTS)

def on_issued_documents_info(req):
    return _categories_in_use(req, "issued_documents")

def on_issued_document_get(req):
    return _crud_get(req, "issued_documents", "Document")

def on_issued_document_modify(req):
    return _crud_modify(req, "issued_documents", "Document")

def on_issued_document_delete(req):
    return _crud_delete(req, "issued_documents", "Document")
