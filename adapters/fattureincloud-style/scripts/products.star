# Product handlers.

_PRODUCT_DEFAULTS = {"name": "", "code": "", "net_price": "0.00", "gross_price": "0.00"}

def on_products_list(req):
    return _crud_list(req, "products")

def on_products_create(req):
    return _crud_create(req, "products", _PRODUCT_DEFAULTS)

def on_product_get(req):
    return _crud_get(req, "products", "Product")

def on_product_modify(req):
    return _crud_modify(req, "products", "Product")

def on_product_delete(req):
    return _crud_delete(req, "products", "Product")
