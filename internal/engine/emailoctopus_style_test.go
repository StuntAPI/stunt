package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestEmailoctopusStyleAdapter exercises the EmailOctopus-style adapter
// end-to-end through the real engine, covering auth, lists CRUD, the contact
// status lifecycle (double opt-in PENDING → SUBSCRIBED → UNSUBSUBSCRIBED →
// SUBSCRIBED again), list filters, pagination, tags, fields, campaigns and
// reports, automations, error shapes, and the catch-all 404.
//
// The adapter's seeded lists include one double-opt-in list
// ("seed-list-doi-newsletter") and one single-opt-in list
// ("seed-list-single-optin") — double opt-in is dashboard-configured in the
// real product, so it can only arrive here via the seed fixture.
func TestEmailoctopusStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "emailoctopus-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"emailoctopus": {Adapter: absAdapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer e.Close()

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	base := addrs["emailoctopus"]
	const token = "eo_local_dev_key"

	// ===== Auth negative: no bearer → 401 in the real problem+json shape =====

	body, status := getAuth(t, base+"/lists", "")
	if status != 401 {
		t.Fatalf("no bearer -> status %d, want 401; body %s", status, body)
	}
	errBody := emailoctopusDecode(t, body)
	emailoctopusAssertProblem(t, errBody, 401, "unauthorized", "Invalid key.")

	// ===== Lists: create → get → list → update =====

	body, status = postJSONAuth(t, base+"/lists", token, map[string]any{"name": "New clients list"})
	if status != 201 {
		t.Fatalf("create list -> status %d, want 201; body %s", status, body)
	}
	created := emailoctopusDecode(t, body)
	listID, ok := created["id"].(string)
	if !ok || listID == "" {
		t.Fatalf("created list id = %v, want non-empty string", created["id"])
	}
	if created["name"] != "New clients list" {
		t.Fatalf("created list name = %v", created["name"])
	}
	if created["double_opt_in"] != false {
		t.Fatalf("created list double_opt_in = %v, want false", created["double_opt_in"])
	}
	emailoctopusAssertRecentISO(t, created["created_at"], "list created_at")

	body, status = getAuth(t, base+"/lists/"+listID, token)
	if status != 200 {
		t.Fatalf("get list -> status %d, want 200; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["id"]; got != listID {
		t.Fatalf("get list id = %v, want %v", got, listID)
	}

	body, status = getAuth(t, base+"/lists", token)
	if status != 200 {
		t.Fatalf("list lists -> status %d, want 200; body %s", status, body)
	}
	listResp := emailoctopusDecode(t, body)
	listData, ok := listResp["data"].([]any)
	if !ok || len(listData) < 3 { // 2 seeded + 1 created
		t.Fatalf("list lists data = %v, want >= 3 entries", listResp["data"])
	}
	// The seeded double-opt-in list must be present with its derived counts
	// (a plain object per the v2 spec, not a wrapped array).
	var doiCounts map[string]any
	for _, it := range listData {
		l := it.(map[string]any)
		if l["id"] == "seed-list-doi-newsletter" {
			counts, ok := l["counts"].(map[string]any)
			if !ok {
				t.Fatalf("list counts = %v, want a plain object", l["counts"])
			}
			doiCounts = counts
		}
	}
	if doiCounts == nil {
		t.Fatalf("seeded double opt-in list missing from GET /lists: %v", listData)
	}
	for _, k := range []string{"pending", "subscribed", "unsubscribed"} {
		if _, ok := doiCounts[k]; !ok {
			t.Fatalf("list counts missing %q: %v", k, doiCounts)
		}
	}

	body, status = emailoctopusPutJSON(t, base+"/lists/"+listID, token, map[string]any{"name": "Renamed clients list"})
	if status != 200 {
		t.Fatalf("update list -> status %d, want 200; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["name"]; got != "Renamed clients list" {
		t.Fatalf("updated list name = %v, want Renamed clients list", got)
	}

	// ===== Contacts on the double-opt-in list: PENDING flow =====

	body, status = postJSONAuth(t, base+"/lists/seed-list-doi-newsletter/contacts", token, map[string]any{
		"email_address": "ada@synth.example",
	})
	if status != 201 {
		t.Fatalf("create DOI contact -> status %d, want 201; body %s", status, body)
	}
	ada := emailoctopusDecode(t, body)
	if ada["status"] != "pending" {
		t.Fatalf("DOI contact status = %v, want pending (double opt-in)", ada["status"])
	}
	adaID, _ := ada["id"].(string)
	if len(adaID) != 32 {
		t.Fatalf("contact id = %q, want 32-char hash of the email", adaID)
	}

	// Status filter: only the pending contact.
	body, status = getAuth(t, base+"/lists/seed-list-doi-newsletter/contacts?status=pending", token)
	if status != 200 {
		t.Fatalf("filter pending -> status %d, want 200; body %s", status, body)
	}
	pend := emailoctopusDecode(t, body)["data"].([]any)
	if len(pend) != 1 {
		t.Fatalf("pending filter count = %d, want 1; body %s", len(pend), body)
	}
	body, status = getAuth(t, base+"/lists/seed-list-doi-newsletter/contacts?status=subscribed", token)
	if status != 200 {
		t.Fatalf("filter subscribed -> status %d, want 200; body %s", status, body)
	}
	if subs := emailoctopusDecode(t, body)["data"].([]any); len(subs) != 0 {
		t.Fatalf("subscribed filter count = %d, want 0", len(subs))
	}

	// Confirm → subscribed, then unsubscribe, then resubscribe.
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-doi-newsletter/contacts/"+adaID, token,
		map[string]any{"status": "subscribed"})
	if status != 200 {
		t.Fatalf("confirm contact -> status %d, want 200; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["status"]; got != "subscribed" {
		t.Fatalf("confirmed status = %v, want subscribed", got)
	}
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-doi-newsletter/contacts/"+adaID, token,
		map[string]any{"status": "unsubscribed"})
	if status != 200 {
		t.Fatalf("unsubscribe -> status %d, want 200; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["status"]; got != "unsubscribed" {
		t.Fatalf("unsubscribed status = %v, want unsubscribed", got)
	}
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-doi-newsletter/contacts/"+adaID, token,
		map[string]any{"status": "subscribed"})
	if status != 200 {
		t.Fatalf("resubscribe -> status %d, want 200; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["status"]; got != "subscribed" {
		t.Fatalf("resubscribed status = %v, want subscribed", got)
	}

	// ===== Contacts on the single-opt-in list =====

	body, status = postJSONAuth(t, base+"/lists/seed-list-single-optin/contacts", token, map[string]any{
		"email_address": "grace@synth.example",
		"tags":          []string{"vip"},
	})
	if status != 201 {
		t.Fatalf("create contact -> status %d, want 201; body %s", status, body)
	}
	grace := emailoctopusDecode(t, body)
	if grace["status"] != "subscribed" {
		t.Fatalf("single opt-in contact status = %v, want subscribed", grace["status"])
	}
	graceID, _ := grace["id"].(string)
	if tags, _ := grace["tags"].([]any); len(tags) != 1 || tags[0] != "vip" {
		t.Fatalf("created contact tags = %v, want [vip]", grace["tags"])
	}

	// Duplicate email → 409 in the conflict problem shape.
	body, status = postJSONAuth(t, base+"/lists/seed-list-single-optin/contacts", token, map[string]any{
		"email_address": "grace@synth.example",
	})
	if status != 409 {
		t.Fatalf("duplicate contact -> status %d, want 409; body %s", status, body)
	}
	emailoctopusAssertProblem(t, emailoctopusDecode(t, body), 409, "conflict", "Resource already exists.")

	// Unknown list → 404.
	body, status = postJSONAuth(t, base+"/lists/does-not-exist/contacts", token, map[string]any{
		"email_address": "x@synth.example",
	})
	if status != 404 {
		t.Fatalf("contact on unknown list -> status %d, want 404; body %s", status, body)
	}
	emailoctopusAssertProblem(t, emailoctopusDecode(t, body), 404, "not-found", "Resource not found.")

	// Validation: missing email → 422 with a JSON Pointer.
	body, status = postJSONAuth(t, base+"/lists/seed-list-single-optin/contacts", token, map[string]any{})
	if status != 422 {
		t.Fatalf("missing email -> status %d, want 422; body %s", status, body)
	}
	valErr := emailoctopusDecode(t, body)
	emailoctopusAssertProblem(t, valErr, 422, "unprocessable-content", "Unprocessable content.")
	if members, _ := valErr["errors"].([]any); len(members) != 1 {
		t.Fatalf("422 errors = %v, want one member", valErr["errors"])
	} else {
		m := members[0].(map[string]any)
		if m["pointer"] != "/email_address" {
			t.Fatalf("422 pointer = %v, want /email_address", m["pointer"])
		}
	}

	// Validation: malformed email → 422.
	body, status = postJSONAuth(t, base+"/lists/seed-list-single-optin/contacts", token, map[string]any{
		"email_address": "not-an-email",
	})
	if status != 422 {
		t.Fatalf("bad email -> status %d, want 422; body %s", status, body)
	}

	// Malformed JSON body → 400 bad-request.
	body, status = emailoctopusPostRaw(t, base+"/lists/seed-list-single-optin/contacts", token, []byte("{\"email_address\": "))
	if status != 400 {
		t.Fatalf("malformed body -> status %d, want 400; body %s", status, body)
	}
	emailoctopusAssertProblem(t, emailoctopusDecode(t, body), 400, "bad-request", "Bad request.")

	// Get a single contact.
	body, status = getAuth(t, base+"/lists/seed-list-single-optin/contacts/"+graceID, token)
	if status != 200 {
		t.Fatalf("get contact -> status %d, want 200; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["email_address"]; got != "grace@synth.example" {
		t.Fatalf("get contact email = %v", got)
	}
	body, status = getAuth(t, base+"/lists/seed-list-single-optin/contacts/deadbeefdeadbeefdeadbeefdeadbeef", token)
	if status != 404 {
		t.Fatalf("get unknown contact -> status %d, want 404; body %s", status, body)
	}

	// Update: fields + tags object (true adds, false removes).
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-single-optin/contacts/"+graceID, token, map[string]any{
		"fields": map[string]any{"Hometown": "Lisbon"},
		"tags":   map[string]any{"vip": false, "customer": true},
	})
	if status != 200 {
		t.Fatalf("update contact -> status %d, want 200; body %s", status, body)
	}
	updated := emailoctopusDecode(t, body)
	if fields, _ := updated["fields"].(map[string]any); fields["Hometown"] != "Lisbon" {
		t.Fatalf("updated fields = %v, want Hometown=Lisbon", updated["fields"])
	}
	if tags, _ := updated["tags"].([]any); len(tags) != 1 || tags[0] != "customer" {
		t.Fatalf("updated tags = %v, want [customer] (vip removed)", updated["tags"])
	}
	if got := updated["id"]; got != graceID {
		t.Fatalf("updated contact id = %v, want unchanged %v", got, graceID)
	}

	// Upsert (PUT on the collection): existing email updates, new email creates.
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-single-optin/contacts", token, map[string]any{
		"email_address": "grace@synth.example",
		"status":        "unsubscribed",
	})
	if status != 200 {
		t.Fatalf("upsert existing -> status %d, want 200; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["status"]; got != "unsubscribed" {
		t.Fatalf("upserted status = %v, want unsubscribed", got)
	}
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-single-optin/contacts", token, map[string]any{
		"email_address": "linus@synth.example",
	})
	if status != 200 {
		t.Fatalf("upsert new -> status %d, want 200; body %s", status, body)
	}
	linus := emailoctopusDecode(t, body)
	if linus["status"] != "subscribed" {
		t.Fatalf("upserted new contact status = %v, want subscribed", linus["status"])
	}
	linusID, _ := linus["id"].(string)

	// Batch update: one good row + one unknown row.
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-single-optin/contacts/batch", token, map[string]any{
		"contacts": []map[string]any{
			{"id": graceID, "status": "subscribed"},
			{"id": "ffffffffffffffffffffffffffffffff"},
		},
	})
	if status != 200 {
		t.Fatalf("batch update -> status %d, want 200; body %s", status, body)
	}
	batch := emailoctopusDecode(t, body)
	if succ, _ := batch["success"].([]any); len(succ) != 1 {
		t.Fatalf("batch success = %v, want one entry; body %s", batch["success"], body)
	} else if row := succ[0].(map[string]any); row["success"] != true {
		t.Fatalf("batch success row = %v, want success true", row)
	}
	if errs, _ := batch["errors"].([]any); len(errs) != 1 {
		t.Fatalf("batch errors = %v, want one entry", batch["errors"])
	} else if row := errs[0].(map[string]any); row["status"] != float64(404) {
		t.Fatalf("batch error row status = %v, want 404", row["status"])
	}

	// ===== Contact tags (v2 surface: contact members + the tag filter) =====
	// NOTE: v2 has NO tag CRUD endpoints (those are legacy 1.6 only) — the
	// adapter deliberately does not serve /lists/{id}/tags.

	body, status = getAuth(t, base+"/lists/seed-list-single-optin/tags", token)
	if status != 404 {
		t.Fatalf("GET /lists/{id}/tags -> status %d, want 404 (not v2 surface); body %s", status, body)
	}

	// Tag filter on contacts (tags is an array member, filtered by the adapter).
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-single-optin/contacts/"+graceID, token,
		map[string]any{"tags": map[string]any{"newsletter": true}})
	if status != 200 {
		t.Fatalf("tag contact -> status %d, want 200; body %s", status, body)
	}
	body, status = getAuth(t, base+"/lists/seed-list-single-optin/contacts?tag=newsletter", token)
	if status != 200 {
		t.Fatalf("filter by tag -> status %d, want 200; body %s", status, body)
	}
	if rows := emailoctopusDecode(t, body)["data"].([]any); len(rows) != 1 {
		t.Fatalf("tag filter count = %d, want 1", len(rows))
	}

	// ===== Fields: create → duplicate → update → delete =====

	body, status = postJSONAuth(t, base+"/lists/seed-list-single-optin/fields", token, map[string]any{
		"label":   "Favourite fruit",
		"tag":     "Fruit",
		"type":    "choice_single",
		"choices": []string{"apple", "orange"},
	})
	if status != 201 {
		t.Fatalf("create field -> status %d, want 201; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["type"]; got != "choice_single" {
		t.Fatalf("created field type = %v", got)
	}
	body, status = postJSONAuth(t, base+"/lists/seed-list-single-optin/fields", token, map[string]any{
		"label": "Favourite fruit", "tag": "Fruit", "type": "text",
	})
	if status != 409 {
		t.Fatalf("duplicate field -> status %d, want 409; body %s", status, body)
	}
	// A choice field without choices → 422.
	body, status = postJSONAuth(t, base+"/lists/seed-list-single-optin/fields", token, map[string]any{
		"label": "Meal", "tag": "Meal", "type": "choice_single",
	})
	if status != 422 {
		t.Fatalf("choice field without choices -> status %d, want 422; body %s", status, body)
	}
	body, status = emailoctopusPutJSON(t, base+"/lists/seed-list-single-optin/fields/Fruit", token, map[string]any{
		"label": "Favourite fruit", "tag": "Fruit", "type": "choice_multiple",
		"choices": []string{"apple", "orange", "pear"},
	})
	if status != 200 {
		t.Fatalf("update field -> status %d, want 200; body %s", status, body)
	}
	body, status = deleteAuth(t, base+"/lists/seed-list-single-optin/fields/Fruit", token)
	if status != 204 {
		t.Fatalf("delete field -> status %d, want 204; body %s", status, body)
	}

	// ===== Pagination: limit + starting_after cursor =====

	for _, email := range []string{"p1@synth.example", "p2@synth.example", "p3@synth.example"} {
		body, status = postJSONAuth(t, base+"/lists/seed-list-doi-newsletter/contacts", token, map[string]any{
			"email_address": email, "status": "subscribed",
		})
		if status != 201 {
			t.Fatalf("create paging contact %s -> status %d; body %s", email, status, body)
		}
	}
	body, status = getAuth(t, base+"/lists/seed-list-doi-newsletter/contacts?limit=2", token)
	if status != 200 {
		t.Fatalf("page 1 -> status %d, want 200; body %s", status, body)
	}
	page1 := emailoctopusDecode(t, body)
	if rows := page1["data"].([]any); len(rows) != 2 {
		t.Fatalf("page 1 count = %d, want 2", len(rows))
	}
	paging, ok := page1["paging"].(map[string]any)
	if !ok {
		t.Fatalf("page 1 paging = %v, want a next envelope", page1["paging"])
	}
	next, _ := paging["next"].(map[string]any)
	cursor, _ := next["starting_after"].(string)
	if cursor == "" {
		t.Fatalf("page 1 next.starting_after = %v, want a cursor", next)
	}
	if url, _ := next["url"].(string); url == "" {
		t.Fatalf("page 1 next.url = %v, want a url", next)
	}
	body, status = getAuth(t, base+"/lists/seed-list-doi-newsletter/contacts?limit=2&starting_after="+cursor, token)
	if status != 200 {
		t.Fatalf("page 2 -> status %d, want 200; body %s", status, body)
	}
	page2 := emailoctopusDecode(t, body)
	if rows := page2["data"].([]any); len(rows) != 2 {
		t.Fatalf("page 2 count = %d, want 2 (4 contacts total)", len(rows))
	}
	if _, hasPaging := page2["paging"]; hasPaging {
		t.Fatalf("page 2 paging = %v, want absent (last page)", page2["paging"])
	}
	// A bogus cursor answers 400, not 500.
	body, status = getAuth(t, base+"/lists/seed-list-doi-newsletter/contacts?starting_after=!!!bogus", token)
	if status != 400 {
		t.Fatalf("bogus cursor -> status %d, want 400; body %s", status, body)
	}

	// ===== Campaigns (read-only; derived on first read) =====

	body, status = getAuth(t, base+"/campaigns", token)
	if status != 200 {
		t.Fatalf("list campaigns -> status %d, want 200; body %s", status, body)
	}
	campData := emailoctopusDecode(t, body)["data"].([]any)
	if len(campData) < 2 {
		t.Fatalf("campaigns count = %d, want >= 2", len(campData))
	}
	var sentID string
	for _, it := range campData {
		c := it.(map[string]any)
		if c["status"] == "sent" {
			sentID, _ = c["id"].(string)
		}
	}
	if sentID == "" {
		t.Fatalf("no sent campaign in %v", campData)
	}
	first, _ := campData[0].(map[string]any)
	if _, ok := first["from"].(map[string]any); !ok {
		t.Fatalf("campaign from = %v, want an object", first["from"])
	}

	body, status = getAuth(t, base+"/campaigns/"+sentID, token)
	if status != 200 {
		t.Fatalf("get campaign -> status %d, want 200; body %s", status, body)
	}
	if got := emailoctopusDecode(t, body)["id"]; got != sentID {
		t.Fatalf("get campaign id = %v, want %v", got, sentID)
	}

	body, status = getAuth(t, base+"/campaigns/"+sentID+"/reports/summary", token)
	if status != 200 {
		t.Fatalf("summary report -> status %d, want 200; body %s", status, body)
	}
	summary := emailoctopusDecode(t, body)
	if v, ok := summary["sent"].(float64); !ok || v < 1 {
		t.Fatalf("summary sent = %v, want >= 1", summary["sent"])
	}
	if _, ok := summary["bounced"].(map[string]any); !ok {
		t.Fatalf("summary bounced = %v, want {hard, soft}", summary["bounced"])
	}

	body, status = getAuth(t, base+"/campaigns/"+sentID+"/reports/links", token)
	if status != 200 {
		t.Fatalf("links report -> status %d, want 200; body %s", status, body)
	}
	if rows := emailoctopusDecode(t, body)["data"].([]any); len(rows) < 1 {
		t.Fatalf("links report data = %v, want >= 1 link", body)
	}

	body, status = getAuth(t, base+"/campaigns/"+sentID+"/reports?status=opened", token)
	if status != 200 {
		t.Fatalf("contact report -> status %d, want 200; body %s", status, body)
	}
	report := emailoctopusDecode(t, body)
	if report["status"] != "opened" {
		t.Fatalf("contact report status = %v, want opened", report["status"])
	}
	if rows := report["data"].([]any); len(rows) < 1 {
		t.Fatalf("contact report data = %v, want >= 1 event", body)
	}
	// ?status= is required by the real endpoint.
	body, status = getAuth(t, base+"/campaigns/"+sentID+"/reports", token)
	if status != 422 {
		t.Fatalf("report without status -> status %d, want 422; body %s", status, body)
	}
	// Unknown campaign → 404.
	body, status = getAuth(t, base+"/campaigns/11111111-2222-4333-8444-555555555555/reports/summary", token)
	if status != 404 {
		t.Fatalf("unknown campaign report -> status %d, want 404; body %s", status, body)
	}

	// ===== Automations: queue a contact (204) =====

	body, status = emailoctopusPostRaw(t, base+"/automations/12345678-1234-4234-8234-123456789012/queue", token,
		[]byte(`{"contact_id": "`+graceID+`"}`))
	if status != 204 {
		t.Fatalf("queue automation -> status %d, want 204; body %s", status, body)
	}
	// Malformed automation id → 404.
	body, status = emailoctopusPostRaw(t, base+"/automations/not-a-uuid/queue", token,
		[]byte(`{"contact_id": "`+graceID+`"}`))
	if status != 404 {
		t.Fatalf("queue bad automation -> status %d, want 404; body %s", status, body)
	}
	// Unknown contact → 404; missing contact_id → 422.
	body, status = emailoctopusPostRaw(t, base+"/automations/12345678-1234-4234-8234-123456789012/queue", token,
		[]byte(`{"contact_id": "ffffffffffffffffffffffffffffffff"}`))
	if status != 404 {
		t.Fatalf("queue unknown contact -> status %d, want 404; body %s", status, body)
	}
	body, status = emailoctopusPostRaw(t, base+"/automations/12345678-1234-4234-8234-123456789012/queue", token,
		[]byte(`{}`))
	if status != 422 {
		t.Fatalf("queue missing contact_id -> status %d, want 422; body %s", status, body)
	}

	// ===== Contact delete → 204, then 404 =====

	body, status = deleteAuth(t, base+"/lists/seed-list-single-optin/contacts/"+linusID, token)
	if status != 204 {
		t.Fatalf("delete contact -> status %d, want 204; body %s", status, body)
	}
	body, status = getAuth(t, base+"/lists/seed-list-single-optin/contacts/"+linusID, token)
	if status != 404 {
		t.Fatalf("get deleted contact -> status %d, want 404; body %s", status, body)
	}

	// ===== Same email across lists (per-list contact identity) =====

	// The contact id is the email hash, but identity is PER LIST: the same
	// address may join a second list without a PK clash, and each list
	// addresses its own row.
	crossListID := "seed-list-doi-newsletter"
	crossBody, status := postJSONAuth(t, base+"/lists/seed-list-single-optin/contacts", token, map[string]any{
		"email_address": "shared@example.test",
	})
	if status != 201 {
		t.Fatalf("create shared contact on list A -> status %d; body %s", status, crossBody)
	}
	sharedA := emailoctopusDecode(t, crossBody)["id"].(string)
	crossBody, status = postJSONAuth(t, base+"/lists/"+crossListID+"/contacts", token, map[string]any{
		"email_address": "shared@example.test",
	})
	if status != 201 {
		t.Fatalf("same email on list B -> status %d (was a PK-clash 500), want 201; body %s", status, crossBody)
	}
	sharedB := emailoctopusDecode(t, crossBody)["id"].(string)
	if sharedA != sharedB {
		t.Fatalf("shared contact ids differ per list: %v vs %v (public id is the email hash)", sharedA, sharedB)
	}
	// Changing list B's copy to an email that exists on list A rekeys within
	// list A's row space only — 409 on the SAME list, free across lists.
	crossBody, status = emailoctopusPutJSON(t, base+"/lists/"+crossListID+"/contacts/"+sharedB, token,
		map[string]any{"email_address": "moved@example.test"})
	if status != 200 {
		t.Fatalf("email change on other list -> status %d, want 200; body %s", status, crossBody)
	}
	body, status = getAuth(t, base+"/lists/seed-list-single-optin/contacts/"+sharedA, token)
	if status != 200 {
		t.Fatalf("list A contact after other list's rekey -> status %d, want 200 (must be untouched)", status)
	}

	// ===== List delete → 204, cascade removes its contacts =====

	// Seed a contact on listID first so the cascade has something to remove.
	body, status = postJSONAuth(t, base+"/lists/"+listID+"/contacts", token, map[string]any{
		"email_address": "cascading@example.test",
	})
	if status != 201 {
		t.Fatalf("create cascade contact -> status %d; body %s", status, body)
	}
	cascadeID := emailoctopusDecode(t, body)["id"].(string)
	body, status = deleteAuth(t, base+"/lists/"+listID, token)
	if status != 204 {
		t.Fatalf("delete list -> status %d, want 204; body %s", status, body)
	}
	body, status = getAuth(t, base+"/lists/"+listID, token)
	if status != 404 {
		t.Fatalf("get deleted list -> status %d, want 404; body %s", status, body)
	}
	body, status = getAuth(t, base+"/lists/"+listID+"/contacts/"+cascadeID, token)
	if status != 404 {
		t.Fatalf("contact after list delete -> status %d, want 404 (cascade); body %s", status, body)
	}

	// ===== Catch-all 404 in the EmailOctopus problem shape =====

	body, status = getAuth(t, base+"/definitely/not/a/route", token)
	if status != 404 {
		t.Fatalf("catch-all -> status %d, want 404; body %s", status, body)
	}
	emailoctopusAssertProblem(t, emailoctopusDecode(t, body), 404, "not-found", "Resource not found.")
}

// === Helpers (emailoctopus-prefixed; shared helpers live in
// stripe_adapter_test.go: getAuth / postJSONAuth / deleteAuth) ===

// emailoctopusPutJSON performs an HTTP PUT with a Bearer token and a JSON
// body, returning the body + status code.
func emailoctopusPutJSON(t *testing.T, url, token string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// emailoctopusPostRaw performs an HTTP POST with a Bearer token and a raw
// (pre-marshalled) body — used to send deliberately malformed JSON.
func emailoctopusPostRaw(t *testing.T, url, token string, body []byte) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// emailoctopusDecode unmarshals a JSON object response body.
func emailoctopusDecode(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return out
}

// emailoctopusAssertProblem checks an RFC 7807 error envelope: type anchor,
// title, detail, and numeric status all match the real EmailOctopus shape.
func emailoctopusAssertProblem(t *testing.T, body map[string]any, status int, slug, detail string) {
	t.Helper()
	wantType := "https://emailoctopus.com/api-documentation/v2#" + slug
	if body["type"] != wantType {
		t.Fatalf("problem type = %v, want %s", body["type"], wantType)
	}
	if body["title"] != "An error occurred." {
		t.Fatalf("problem title = %v, want \"An error occurred.\"", body["title"])
	}
	if body["detail"] != detail {
		t.Fatalf("problem detail = %v, want %q", body["detail"], detail)
	}
	if v, ok := body["status"].(float64); !ok || int(v) != status {
		t.Fatalf("problem status = %v, want %d", body["status"], status)
	}
}

// emailoctopusHas reports whether the JSON array contains the string s.
func emailoctopusHas(arr []any, s string) bool {
	for _, v := range arr {
		if str, ok := v.(string); ok && str == s {
			return true
		}
	}
	return false
}

// emailoctopusAssertRecentISO checks that v is an ISO 8601 timestamp minted
// within the last 15 minutes — the adapter derives timestamps from the engine
// clock, never a hardcoded literal.
func emailoctopusAssertRecentISO(t *testing.T, v any, what string) {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s = %v, want an ISO 8601 string", what, v)
	}
	ts, err := time.Parse("2006-01-02T15:04:05-07:00", s)
	if err != nil {
		t.Fatalf("%s = %q, want ISO 8601 with numeric offset: %v", what, s, err)
	}
	if d := time.Since(ts); d < -time.Minute || d > 15*time.Minute {
		t.Fatalf("%s = %v, want within 15min of now (age %s)", what, s, d)
	}
}
