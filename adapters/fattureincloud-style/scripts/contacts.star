# Supplier and client handlers — the generic CRUD over a simpler shape.

_SUPPLIER_DEFAULTS = {"name": "", "code": "", "country": "IT", "type": "supplier"}
_CLIENT_DEFAULTS = {"name": "", "code": "", "country": "IT", "type": "client"}

def on_suppliers_list(req):
    return _crud_list(req, "suppliers")

def on_suppliers_create(req):
    return _crud_create(req, "suppliers", _SUPPLIER_DEFAULTS)

def on_supplier_get(req):
    return _crud_get(req, "suppliers", "Contact")

def on_supplier_modify(req):
    return _crud_modify(req, "suppliers", "Contact")

def on_supplier_delete(req):
    return _crud_delete(req, "suppliers", "Contact")

def on_clients_list(req):
    return _crud_list(req, "clients")

def on_clients_create(req):
    return _crud_create(req, "clients", _CLIENT_DEFAULTS)

def on_client_get(req):
    return _crud_get(req, "clients", "Contact")

def on_client_modify(req):
    return _crud_modify(req, "clients", "Contact")

def on_client_delete(req):
    return _crud_delete(req, "clients", "Contact")
