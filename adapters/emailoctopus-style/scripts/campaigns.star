# Campaign handlers — the read-only /campaigns surface plus its reports.
#
#   GET /campaigns                               list campaigns
#   GET /campaigns/{campaign_id}                 get campaign
#   GET /campaigns/{campaign_id}/reports/summary aggregate send report
#   GET /campaigns/{campaign_id}/reports/links   per-link click report
#   GET /campaigns/{campaign_id}/reports         per-contact report
#                                               (?status=<event> REQUIRED)
#
# NOTE ON FIDELITY: the real v2 API exposes NO campaign create/update/delete
# endpoint — campaigns are authored in the EmailOctopus dashboard and are
# read-only over the API. This adapter keeps that route surface exactly
# (no POST /campaigns exists here either). So the simulator has something to
# read, the campaigns collection is DERIVED ON FIRST READ: an empty store
# materialises two synthetic campaigns (one "sent", one "draft"), the same
# derive-on-read pattern stunt uses elsewhere.
#
# Shared helpers are preloaded from scripts/lib.star.

# on_list_campaigns answers GET /campaigns.
def on_list_campaigns(req):
    err = _require_auth(req)
    if err != None:
        return err

    _ensure_campaigns()
    docs = store_collection("campaigns").list()
    docs = query_select(docs, None, "id", "asc", None, None, None)
    docs = query_select(docs, None, "created_at", "asc", None, None, None)
    return _paginated(req, "/campaigns", [_present_campaign(d) for d in docs])

# on_get_campaign answers GET /campaigns/{campaign_id}.
def on_get_campaign(req):
    err = _require_auth(req)
    if err != None:
        return err

    _ensure_campaigns()
    doc = store_collection("campaigns").get(_param(req, "campaign_id"))
    if doc == None:
        return _not_found()
    return respond(200, _present_campaign(doc))

# on_campaign_summary answers GET /campaigns/{campaign_id}/reports/summary —
# the aggregate {sent, bounced{hard,soft}, opened{total,unique},
# clicked{total,unique}, complained, unsubscribed} shape from the v2 schema.
def on_campaign_summary(req):
    err = _require_auth(req)
    if err != None:
        return err

    _ensure_campaigns()
    doc = store_collection("campaigns").get(_param(req, "campaign_id"))
    if doc == None:
        return _not_found()

    stats = doc.get("report", {})
    return respond(200, {
        "id": doc.get("id", ""),
        "sent": stats.get("sent", 0),
        "bounced": {
            "hard": stats.get("bounced_hard", 0),
            "soft": stats.get("bounced_soft", 0),
        },
        "opened": {
            "total": stats.get("opened_total", 0),
            "unique": stats.get("opened_unique", 0),
        },
        "clicked": {
            "total": stats.get("clicked_total", 0),
            "unique": stats.get("clicked_unique", 0),
        },
        "complained": stats.get("complained", 0),
        "unsubscribed": stats.get("unsubscribed", 0),
    })

# on_campaign_links answers GET /campaigns/{campaign_id}/reports/links —
# {"data": [{"url", "clicked_total", "clicked_unique"}]} (no paging member
# in the published schema).
def on_campaign_links(req):
    err = _require_auth(req)
    if err != None:
        return err

    _ensure_campaigns()
    doc = store_collection("campaigns").get(_param(req, "campaign_id"))
    if doc == None:
        return _not_found()

    links = []
    for l in doc.get("links", []):
        links.append({
            "url": l.get("url", ""),
            "clicked_total": _num(l.get("clicked_total", 0), 0),
            "clicked_unique": _num(l.get("clicked_unique", 0), 0),
        })
    return respond(200, {"data": links})

# on_campaign_contact_report answers GET /campaigns/{campaign_id}/reports —
# the per-contact report. ?status= is REQUIRED by the real API and is one of
# bounced | clicked | complained | opened | sent | unsubscribed | not-opened
# | not-clicked. Response: {"status": <event>, "data": [{contact_id,
# contact_email_address, occurred_at}], "paging": {...}}.
def on_campaign_contact_report(req):
    err = _require_auth(req)
    if err != None:
        return err

    _ensure_campaigns()
    doc = store_collection("campaigns").get(_param(req, "campaign_id"))
    if doc == None:
        return _not_found()

    q = _query(req)
    status = q.get("status", "")
    if status == None or status == "":
        return _unprocessable([_perr("status", "This value should not be blank.")])
    if _report_status_ok(status) == False:
        return _unprocessable([
            _perr("status", "The value you selected is not a valid choice."),
        ])

    events = []
    for ev in doc.get("events", []):
        if ev.get("status", "") == status:
            events.append(ev)

    # The report envelope embeds the selected status alongside the data.
    limit, off = _page_params(req)
    if limit == None:
        return _bad_request()
    page, nxt = paginate(events, limit, str(off) if off > 0 else None)
    body = {"status": status, "data": page}
    if nxt != None:
        sa = _cursor(nxt)
        path = "/campaigns/" + doc.get("id", "") + "/reports"
        body["paging"] = {
            "next": {
                "url": _API_HOST + path + "?status=" + status +
                       "&starting_after=" + sa + "&limit=" + str(limit),
                "starting_after": sa,
            },
        }
    return respond(200, body)

# ============================================================================
# INTERNALS
# ============================================================================

# _report_status_ok reports whether s is one of the documented report event
# statuses.
_REPORT_STATUSES = [
    "bounced", "clicked", "complained", "opened", "sent",
    "unsubscribed", "not-opened", "not-clicked",
]

def _report_status_ok(s):
    return s in _REPORT_STATUSES

# _ensure_campaigns materialises the synthetic campaign set on first read.
# Campaigns are dashboard-authored in the real product, so the simulator
# derives them rather than exposing a create endpoint that does not exist.
def _ensure_campaigns():
    cc = store_collection("campaigns")
    if len(cc.list()) > 0:
        return

    now = _iso_now()
    cc.insert({
        "id": _uuid(),
        "status": "sent",
        "name": "Monthly digest",
        "subject": "Your monthly digest",
        "to": [],
        "from": {"name": "Otto Synth", "email_address": "otto@synth.example"},
        "content": {"html": "<html>Monthly digest</html>"},
        "created_at": now,
        "sent_at": now,
        "report": {
            "sent": 12,
            "bounced_hard": 1,
            "bounced_soft": 0,
            "opened_total": 15,
            "opened_unique": 9,
            "clicked_total": 7,
            "clicked_unique": 5,
            "complained": 0,
            "unsubscribed": 1,
        },
        "links": [
            {"url": "https://synth.example/read-more", "clicked_total": 4, "clicked_unique": 3},
            {"url": "https://synth.example/unsubscribe", "clicked_total": 1, "clicked_unique": 1},
        ],
        "events": _seed_events(),
    })
    cc.insert({
        "id": _uuid(),
        "status": "draft",
        "name": "Launch announcement",
        "subject": "Something new",
        "to": [],
        "from": {"name": "Otto Synth", "email_address": "otto@synth.example"},
        "content": {"html": "<html>Launch announcement</html>"},
        "created_at": now,
        "sent_at": None,
        "report": {},
        "links": [],
        "events": [],
    })

# _seed_events builds the synthetic per-contact report events for the seeded
# sent campaign. Email addresses are example-domain synthetics; contact ids
# are derived the same way real contact ids are (hash of the address).
def _seed_events():
    rows = [
        ["sent", "ada@synth.example"],
        ["sent", "grace@synth.example"],
        ["sent", "linus@synth.example"],
        ["opened", "ada@synth.example"],
        ["opened", "grace@synth.example"],
        ["clicked", "ada@synth.example"],
        ["unsubscribed", "grace@synth.example"],
        ["bounced", "linus@synth.example"],
    ]
    now = _iso_now()
    out = []
    for r in rows:
        out.append({
            "status": r[0],
            "contact_id": _contact_id(r[1]),
            "contact_email_address": r[1],
            "occurred_at": now,
        })
    return out

# _present_campaign projects a stored campaign doc into the Campaign-get
# response shape.
def _present_campaign(doc):
    return {
        "id": doc.get("id", ""),
        "status": doc.get("status", ""),
        "name": doc.get("name", ""),
        "subject": doc.get("subject", ""),
        "to": doc.get("to", []),
        "from": doc.get("from", {}),
        "content": doc.get("content", {}),
        "created_at": doc.get("created_at", ""),
        "sent_at": doc.get("sent_at", None),
    }
