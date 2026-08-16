# Credit note handlers — post-payment / pre-payment adjustments against a
# finalized invoice (docs.stripe.com/api/credit_notes).
#
# A credit note first reduces the invoice's amount_remaining (its
# pre_payment_amount); the excess (post_payment_amount) can leave Stripe as
# a real refund against the invoice's charge (refund_amount) and/or as
# customer-balance credit (credit_amount). The refund is created with the
# shared lib helpers (refund doc + balance transaction + refund.created) and
# the charge doc is updated like POST /v1/refunds does.
#
# The classic refund/credit boolean params are accepted alongside the modern
# refund_amount/credit_amount for older SDK integrations.
# Shared helpers (_require_auth, _next_id, _now, _num, _to_int_signed,
# _not_found, _list_page, _newest_first, _created_filters, _created_check,
# _signed_emit, _create_refund, _refunds_for, _refunded_total,
# _apply_charge_refund) are in lib.star.

_CN_COLLECTION = "credit_notes"

_CN_REASONS = ["duplicate", "fraudulent", "order_change", "product_unsatisfactory"]

# _cn_err builds the real Stripe 400 envelope.
def _cn_err(msg, param):
    e = {"type": "invalid_request_error", "message": msg}
    if param != None:
        e["param"] = param
    return respond(400, {"error": e})

# body arrives as an EMPTY dict via req.body; req.raw_body is the truth).
# _cn_reason normalizes the credit-note reason: the four documented values
# pass through, the legacy duplicated/product_unacceptable spellings map to
# their modern forms, anything else is a 400 (or None when absent).
# Returns [reason, err] with err None on success.
def _cn_reason(raw):
    if raw == None or raw == "":
        return None, None
    if raw == "duplicated":
        return "duplicate", None
    if raw == "product_unacceptable":
        return "product_unsatisfactory", None
    for i in range(len(_CN_REASONS)):
        if _CN_REASONS[i] == raw:
            return raw, None
    return None, _cn_err("Invalid enum value: " + str(raw) + ". Allowed values: duplicate, fraudulent, order_change, product_unsatisfactory.", "reason")

# _cn_qint reads an (optionally signed) integer query param.
def _cn_qint(req, key):
    v = _get_query(req, key)
    if v == "":
        return 0
    return _to_int_signed(v)

# _cn_build_lines derives the credit note lines + total from the request:
# either body `lines` (invoice_line_item refs or custom_line_items) or a
# bare `amount`. Invoice line amounts are per-unit (subtotal = amount x
# quantity), matching the INVOICE line contract. Returns [lines, total, err].
def _cn_build_lines(invoice, body):
    raw = body.get("lines", None)
    if raw != None and type(raw) == "list":
        lines = []
        total = 0
        inv_lines = invoice.get("lines", [])
        for i in range(len(raw)):
            entry = raw[i]
            if entry == None or type(entry) != "dict":
                continue
            ltype = entry.get("type", "custom_line_item")
            il = None
            if ltype == "invoice_line_item":
                ref = entry.get("invoice_line_item", None)
                for j in range(len(inv_lines)):
                    if inv_lines[j].get("id", None) == ref:
                        il = inv_lines[j]
                        break
                if il == None:
                    return [], 0, _cn_err("No such line_item: '" + str(ref) + "'", "lines")
                unit = _num(il.get("amount", 0))
                qty = _num(entry.get("quantity", il.get("quantity", 1)))
                if qty < 1:
                    qty = 1
                desc = il.get("description", None)
            else:
                unit = _num(entry.get("unit_amount", 0))
                if entry.get("unit_amount", None) == None:
                    unit = _num(entry.get("amount", 0))
                qty = _num(entry.get("quantity", 1))
                if qty < 1:
                    qty = 1
                desc = entry.get("description", None)
            amt = unit * qty
            total = total + amt
            lines.append({
                "id": _next_id("cnli"),
                "object": "credit_note_line_item",
                "amount": amt,
                "description": desc,
                "discount_amount": 0,
                "discount_amounts": [],
                "invoice_line_item": entry.get("invoice_line_item", None) if ltype == "invoice_line_item" else None,
                "livemode": False,
                "quantity": qty,
                "tax_rates": [],
                "taxes": [],
                "type": ltype,
                "unit_amount": unit,
                "unit_amount_decimal": str(unit),
            })
        return lines, total, None

    amount = _num(body.get("amount", 0))
    if amount <= 0:
        return [], 0, _cn_err("One of `amount`, `lines`, or `shipping_cost` must be set.", "amount")
    return [], amount, None

# _cn_prior_total sums the amounts of every non-voided credit note already
# issued against an invoice (the max-creditable guard).
def _cn_prior_total(invoice_id):
    docs = query_select(store_collection(_CN_COLLECTION).list(), [["invoice", "=", invoice_id]])
    total = 0
    for i in range(len(docs)):
        if docs[i].get("status", "issued") != "voided":
            total = total + _num(docs[i].get("amount", 0))
    return total

# _cn_public renders a stored credit note, wrapping lines in a list object
# and stripping internal keys. Previews carry a null id, so the lines url
# falls back to the collection path.
def _cn_public(doc):
    out = {}
    for k in doc:
        if k.startswith("_"):
            continue
        out[k] = doc[k]
    url = "/v1/credit_notes"
    if doc.get("id", None) != None:
        url = "/v1/credit_notes/" + doc["id"] + "/lines"
    out["lines"] = {
        "object": "list",
        "data": doc.get("lines", []),
        "has_more": False,
        "url": url,
    }
    return out

# _cn_refund_reason maps a credit-note reason onto a real refund reason.
def _cn_refund_reason(reason):
    if reason == "duplicate" or reason == "fraudulent":
        return reason
    return "requested_by_customer"

# _cn_assemble computes the full credit-note shape for a create/preview.
# apply=True executes the money movement (refund + customer-balance credit +
# invoice amount_remaining reduction); apply=False leaves everything
# untouched (preview). Returns [doc, err].
def _cn_assemble(invoice, body, apply):
    total_reason, bad = _cn_reason(body.get("reason", None))
    if bad != None:
        return None, bad

    lines, total, err = _cn_build_lines(invoice, body)
    if err != None:
        return None, err

    remaining = _num(invoice.get("amount_remaining", 0))
    paid = _num(invoice.get("amount_paid", 0))
    limit = remaining + paid - _cn_prior_total(invoice["id"])
    if total > limit:
        return None, _cn_err("Credit note amount (" + _usd(total) + ") is greater than the maximum creditable amount (" + _usd(limit) + ").", "amount")

    pre = total
    if pre > remaining:
        pre = remaining
    post = total - pre

    refund_amount = _num(body.get("refund_amount", 0))
    if refund_amount == 0 and body.get("refund", False) == True:
        refund_amount = post
    credit_amount = _num(body.get("credit_amount", 0))
    if credit_amount == 0 and body.get("credit", False) == True:
        credit_amount = post - refund_amount
    if refund_amount < 0 or credit_amount < 0:
        return None, _cn_err("Credit note amounts must be non-negative.", "amount")
    if refund_amount > post:
        return None, _cn_err("Refund amount (" + _usd(refund_amount) + ") is greater than the post-payment amount (" + _usd(post) + ").", "refund_amount")
    if credit_amount > post - refund_amount:
        return None, _cn_err("Credit amount (" + _usd(credit_amount) + ") is greater than the remaining post-payment amount (" + _usd(post - refund_amount) + ").", "credit_amount")

    refund_ids = []
    cbt = None
    if apply:
        if refund_amount > 0:
            charge_id = invoice.get("charge", None)
            if charge_id == None or charge_id == "":
                return None, _cn_err("This invoice has no charge to refund.", "refund_amount")
            ch = store_collection("charges").get(charge_id)
            if ch == None:
                return None, _not_found("charge", charge_id)
            already = _refunded_total(_refunds_for("charge", charge_id))
            if refund_amount > _num(ch.get("amount", 0)) - already:
                return None, _over_refund_error(refund_amount, _num(ch.get("amount", 0)) - already)
            re_doc = _create_refund(None, charge_id, refund_amount, invoice.get("currency", "usd"), _cn_refund_reason(total_reason), False)
            refund_ids.append(re_doc["id"])
            _apply_charge_refund(ch, already, refund_amount)
            store_collection("charges").update(charge_id, ch)
            _signed_emit("charge.refunded", ch)
        if credit_amount > 0:
            cus_id = invoice.get("customer", None)
            cus = store_collection("customers").get(cus_id)
            if cus == None:
                return None, _not_found("customer", cus_id)
            # Real Stripe customer balance: negative = credit.
            cus["balance"] = _num(cus.get("balance", 0)) - credit_amount
            store_collection("customers").update(cus_id, cus)
            cbt = _next_id("cbt")

    cn_type = "pre_payment"
    if post > 0:
        cn_type = "post_payment"

    doc = {
        "id": _next_id("cn"),
        "object": "credit_note",
        "amount": total,
        "amount_shipping": 0,
        "created": _now(),
        "currency": invoice.get("currency", "usd"),
        "customer": invoice.get("customer", None),
        "customer_balance_transaction": cbt,
        "discount_amount": 0,
        "discount_amounts": [],
        "effective_at": _now(),
        "invoice": invoice["id"],
        "lines": lines,
        "livemode": False,
        "memo": body.get("memo", None),
        "metadata": body.get("metadata", {}),
        "number": "CN-" + str(store_kv_incr("stripe", "cn_seq")),
        "out_of_band_amount": None,
        "pdf": None,
        "pre_payment_amount": pre,
        "post_payment_amount": post,
        "reason": total_reason,
        "refunds": refund_ids,
        "shipping_cost": None,
        "status": "issued",
        "subtotal": total,
        "subtotal_excluding_tax": total,
        "total": total,
        "total_excluding_tax": total,
        "total_taxes": [],
        "type": cn_type,
        "voided_at": None,
        "_voided": False,
    }
    if not apply:
        doc["id"] = None

    if apply:
        invoice["amount_remaining"] = remaining - pre
        if invoice["amount_remaining"] < 0:
            invoice["amount_remaining"] = 0
        if _num(invoice.get("amount_due", 0)) > 0:
            invoice["amount_due"] = remaining - pre
            if invoice["amount_due"] < 0:
                invoice["amount_due"] = 0
        store_collection("invoices").update(invoice["id"], invoice)

    return doc, None

# _cn_load fetches an invoice and enforces the finalized requirement shared
# by create + preview (draft/void/uncollectible invoices cannot be credited).
def _cn_load_invoice(invoice_id):
    inv = store_collection("invoices").get(invoice_id)
    if inv == None:
        return None, _not_found("invoice", invoice_id)
    if inv.get("status", "") not in ["paid", "open"]:
        return None, _cn_err("Credit notes may only be created for finalized invoices with a status of paid or open.", "invoice")
    return inv, None

# POST /v1/credit_notes — issue a credit note against a finalized invoice.
def on_create_credit_note(req):
    err = _require_auth(req)
    if err != None:
        return err

    cached = _idempotent_lookup(req, _CN_COLLECTION)
    if cached != None:
        return respond(cached["status"], _cn_public(cached["doc"]))

    if _bad_body(req):
        return _cn_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    invoice_id = body.get("invoice", None)
    if invoice_id == None or invoice_id == "":
        return _cn_err("Missing required param: invoice.", "invoice")
    invoice, bad = _cn_load_invoice(invoice_id)
    if bad != None:
        return bad

    doc, bad = _cn_assemble(invoice, body, True)
    if bad != None:
        return bad

    store_collection(_CN_COLLECTION).insert(doc)
    _signed_emit("credit_note.created", _cn_public(doc))
    _idempotent_remember(req, _CN_COLLECTION, 201, doc["id"])
    return respond(201, _cn_public(doc))

# GET /v1/credit_notes/preview — compute the credit note WITHOUT persisting
# anything (no refund, no customer-balance change, no event). Query params:
# invoice (required), amount | lines-unfriendly quantities via amount,
# refund_amount, credit_amount, reason.
def on_preview_credit_note(req):
    err = _require_auth(req)
    if err != None:
        return err

    invoice_id = _get_query(req, "invoice")
    if invoice_id == "":
        b = req.get("body", None)
        if b != None and type(b) == "dict" and b.get("invoice", None) != None:
            invoice_id = b["invoice"]
    if invoice_id == "":
        return _cn_err("Missing required param: invoice.", "invoice")
    invoice, bad = _cn_load_invoice(invoice_id)
    if bad != None:
        return bad

    params = {
        "amount": _cn_qint(req, "amount"),
        "refund_amount": _cn_qint(req, "refund_amount"),
        "credit_amount": _cn_qint(req, "credit_amount"),
        "reason": _get_query(req, "reason"),
        "memo": _get_query(req, "memo"),
        "lines": None,
    }
    b = req.get("body", None)
    if b != None and type(b) == "dict":
        for k in ["lines", "reason", "memo"]:
            if b.get(k, None) != None:
                params[k] = b[k]

    doc, bad = _cn_assemble(invoice, params, False)
    if bad != None:
        return bad
    return respond(200, _cn_public(doc))

# GET /v1/credit_notes/{id} — retrieve a credit note.
def on_retrieve_credit_note(req):
    err = _require_auth(req)
    if err != None:
        return err
    doc = store_collection(_CN_COLLECTION).get(req["params"]["id"])
    if doc == None:
        return _not_found("credit_note", req["params"]["id"])
    return respond(200, _cn_public(doc))

# GET /v1/credit_notes — list credit notes (customer, invoice, created).
def on_list_credit_notes(req):
    err = _require_auth(req)
    if err != None:
        return err
    bad = _created_check(req)
    if bad != None:
        return bad
    f = []
    cust = _get_query(req, "customer")
    if cust != "":
        f.append(["customer", "=", cust])
    inv = _get_query(req, "invoice")
    if inv != "":
        f.append(["invoice", "=", inv])
    _created_filters(req, f)
    docs = store_collection(_CN_COLLECTION).list()
    if len(f) > 0:
        docs = query_select(docs, f)
    docs = _newest_first(docs)
    page, has_more, e = _list_page(req, docs, "credit_note")
    if e != None:
        return e
    return respond(200, {"object": "list", "data": [_cn_public(d) for d in page], "has_more": has_more, "url": "/v1/credit_notes"})

# POST /v1/credit_notes/{id} — update a credit note (metadata/memo only,
# like the real API).
def on_update_credit_note(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_CN_COLLECTION).get(id)
    if doc == None:
        return _not_found("credit_note", id)

    if _bad_body(req):
        return _cn_err("Invalid request body: could not parse as JSON.", None)
    body = req["body"]
    if body == None:
        body = {}

    if body.get("metadata", None) != None and type(body["metadata"]) == "dict":
        meta = doc.get("metadata", {})
        if meta == None or type(meta) != "dict":
            meta = {}
        for k in body["metadata"]:
            meta[k] = body["metadata"][k]
        doc["metadata"] = meta
    if body.get("memo", None) != None:
        doc["memo"] = body["memo"]

    store_collection(_CN_COLLECTION).update(id, doc)
    _signed_emit("credit_note.updated", _cn_public(doc))
    return respond(200, _cn_public(doc))

# POST /v1/credit_notes/{id}/void — void an issued credit note.
def on_void_credit_note(req):
    err = _require_auth(req)
    if err != None:
        return err
    id = req["params"]["id"]
    doc = store_collection(_CN_COLLECTION).get(id)
    if doc == None:
        return _not_found("credit_note", id)
    if doc.get("status", "") != "issued":
        return _cn_err("You cannot void this credit note because it has a status of " + doc.get("status", "") + ". Only issued credit notes may be voided.", None)

    doc["status"] = "voided"
    doc["voided_at"] = _now()
    doc["_voided"] = True
    store_collection(_CN_COLLECTION).update(id, doc)
    _signed_emit("credit_note.voided", _cn_public(doc))
    return respond(200, _cn_public(doc))
