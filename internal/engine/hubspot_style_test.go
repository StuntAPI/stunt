package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestHubspotStyleAdapter exercises the HubSpot-style adapter end-to-end:
//
//   - Bearer auth → list contacts (cursor pagination)
//   - create contact
//   - PATCH contact
//   - GET contact by id
//   - associate contact to company
//   - list companies
//   - batch read contacts
//   - DELETE contact → archived: 404 by default, visible with ?archived=true
//   - 401 without auth → HubSpot error envelope
func TestHubspotStyleAdapter(t *testing.T) {
	base := hsStart(t)

	const token = "pat-mock-token"

	// ===== list contacts (cursor pagination) =====

	body, status := hsAuthGet(t, base+"/crm/v3/objects/contacts", token)
	if status != 200 {
		t.Fatalf("list contacts -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	results, ok := listResp["results"].([]any)
	if !ok || len(results) == 0 {
		t.Fatalf("results = %v, want non-empty array", listResp["results"])
	}
	// Verify the contact shape.
	contact0 := results[0].(map[string]any)
	if _, ok := contact0["id"].(string); !ok {
		t.Fatalf("contact id = %v, want string", contact0["id"])
	}
	props, ok := contact0["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %v, want object", contact0["properties"])
	}
	if _, ok := props["firstname"].(string); !ok {
		t.Fatalf("firstname = %v, want string", props["firstname"])
	}
	// Should have paging field (present, may be null at end of results).
	if _, ok := listResp["paging"]; !ok {
		t.Fatalf("paging field missing from response: %v", listResp)
	}

	// ===== create contact =====

	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts", token, map[string]any{
		"properties": map[string]any{
			"firstname": "John",
			"lastname":  "Smith",
			"email":     "john.smith@example.com",
		},
	})
	if status != 201 {
		t.Fatalf("create contact -> %d, want 201; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	contactID, ok := createResp["id"].(string)
	if !ok || contactID == "" {
		t.Fatalf("id = %v, want non-empty string", createResp["id"])
	}
	contactProps, ok := createResp["properties"].(map[string]any)
	if !ok {
		t.Fatalf("created contact properties = %v, want object", createResp["properties"])
	}
	if contactProps["firstname"] != "John" {
		t.Fatalf("firstname = %v, want 'John'", contactProps["firstname"])
	}
	if _, ok := createResp["createdAt"].(string); !ok {
		t.Fatalf("createdAt = %v, want string", createResp["createdAt"])
	}

	// ===== PATCH contact =====

	body, status = hsAuthPatchJSON(t, base+"/crm/v3/objects/contacts/"+contactID, token, map[string]any{
		"properties": map[string]any{
			"firstname": "Johnny",
			"lastname":  "Smith",
		},
	})
	if status != 200 {
		t.Fatalf("patch contact -> %d, want 200; body %s", status, body)
	}
	var patchResp map[string]any
	if err := json.Unmarshal([]byte(body), &patchResp); err != nil {
		t.Fatalf("unmarshal patch resp: %v (body %s)", err, body)
	}
	patchProps := patchResp["properties"].(map[string]any)
	if patchProps["firstname"] != "Johnny" {
		t.Fatalf("patched firstname = %v, want 'Johnny'", patchProps["firstname"])
	}

	// ===== GET contact by id =====

	body, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID, token)
	if status != 200 {
		t.Fatalf("get contact -> %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal get resp: %v (body %s)", err, body)
	}
	if getResp["id"] != contactID {
		t.Fatalf("retrieved id = %v, want %s", getResp["id"], contactID)
	}

	// ===== list companies =====

	body, status = hsAuthGet(t, base+"/crm/v3/objects/companies", token)
	if status != 200 {
		t.Fatalf("list companies -> %d, want 200; body %s", status, body)
	}
	var companyResp map[string]any
	if err := json.Unmarshal([]byte(body), &companyResp); err != nil {
		t.Fatalf("unmarshal companies: %v (body %s)", err, body)
	}
	companyResults, ok := companyResp["results"].([]any)
	if !ok || len(companyResults) == 0 {
		t.Fatalf("companies results = %v, want non-empty", companyResp["results"])
	}
	company0 := companyResults[0].(map[string]any)
	companyID, _ := company0["id"].(string)
	if companyID == "" {
		t.Fatalf("company id = %v, want non-empty", company0["id"])
	}

	// ===== associate contact to company (PUT → 204, idempotent) =====

	_, status = hsAuthPut(t, base+"/crm/v3/objects/contacts/"+contactID+
		"/associations/company/"+companyID+"/contact_to_company", token)
	if status != 204 {
		t.Fatalf("associate -> %d, want 204", status)
	}
	// Second PUT is a no-op, not a duplicate.
	_, status = hsAuthPut(t, base+"/crm/v3/objects/contacts/"+contactID+
		"/associations/company/"+companyID+"/contact_to_company", token)
	if status != 204 {
		t.Fatalf("idempotent associate -> %d, want 204", status)
	}

	// Verify association stored (v3 read shape: {id, type}).
	body, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID+
		"/associations/company", token)
	if status != 200 {
		t.Fatalf("get associations -> %d, want 200; body %s", status, body)
	}
	var assocResp map[string]any
	if err := json.Unmarshal([]byte(body), &assocResp); err != nil {
		t.Fatalf("unmarshal associations: %v (body %s)", err, body)
	}
	assocResults, ok := assocResp["results"].([]any)
	if !ok || len(assocResults) != 1 {
		t.Fatalf("association results = %v, want exactly 1", assocResp["results"])
	}
	assoc0 := assocResults[0].(map[string]any)
	if assoc0["id"] != companyID {
		t.Fatalf("association id = %v, want %s", assoc0["id"], companyID)
	}
	if assoc0["type"] != "contact_to_company" {
		t.Fatalf("association type = %v, want contact_to_company", assoc0["type"])
	}

	// ===== DELETE the association → 204, then the listing is empty =====

	_, status = hsAuthDelete(t, base+"/crm/v3/objects/contacts/"+contactID+
		"/associations/company/"+companyID+"/contact_to_company", token)
	if status != 204 {
		t.Fatalf("delete association -> %d, want 204", status)
	}
	body, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID+
		"/associations/company", token)
	if status != 200 {
		t.Fatalf("get associations after delete -> %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &assocResp); err != nil {
		t.Fatalf("unmarshal associations after delete: %v", err)
	}
	if assocResults, ok := assocResp["results"].([]any); !ok || len(assocResults) != 0 {
		t.Fatalf("association results after delete = %v, want empty", assocResp["results"])
	}

	// ===== batch read contacts =====

	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/batch/read", token, map[string]any{
		"properties": []string{"firstname", "lastname", "email"},
		"inputs": []map[string]any{
			{"id": contactID},
		},
	})
	if status != 200 {
		t.Fatalf("batch read -> %d, want 200; body %s", status, body)
	}
	var batchResp map[string]any
	if err := json.Unmarshal([]byte(body), &batchResp); err != nil {
		t.Fatalf("unmarshal batch resp: %v (body %s)", err, body)
	}
	batchResults, ok := batchResp["results"].([]any)
	if !ok || len(batchResults) != 1 {
		t.Fatalf("batch results = %v, want exactly 1", batchResp["results"])
	}
	batchRec := batchResults[0].(map[string]any)
	if batchRec["id"] != contactID {
		t.Fatalf("batch record id = %v, want %s", batchRec["id"], contactID)
	}

	// ===== DELETE contact → ARCHIVE (soft delete) =====

	_, status = hsAuthDelete(t, base+"/crm/v3/objects/contacts/"+contactID, token)
	if status != 204 {
		t.Fatalf("delete contact -> %d, want 204", status)
	}
	// Default GET: archived records 404.
	_, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID, token)
	if status != 404 {
		t.Fatalf("get after delete -> %d, want 404", status)
	}
	// ?archived=true: the archived record is visible again.
	body, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID+"?archived=true", token)
	if status != 200 {
		t.Fatalf("get archived -> %d, want 200; body %s", status, body)
	}
	var archResp map[string]any
	if err := json.Unmarshal([]byte(body), &archResp); err != nil {
		t.Fatalf("unmarshal archived resp: %v (body %s)", err, body)
	}
	if archResp["archived"] != true {
		t.Fatalf("archived = %v, want true", archResp["archived"])
	}
	if _, ok := archResp["archivedAt"].(string); !ok {
		t.Fatalf("archivedAt = %v, want string", archResp["archivedAt"])
	}

	// Restore brings the record back into default reads.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/"+contactID+"/restore", token, map[string]any{})
	if status != 200 {
		t.Fatalf("restore -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &archResp); err != nil {
		t.Fatalf("unmarshal restore resp: %v (body %s)", err, body)
	}
	if archResp["archived"] != false {
		t.Fatalf("restored archived = %v, want false", archResp["archived"])
	}
	_, status = hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contactID, token)
	if status != 200 {
		t.Fatalf("get after restore -> %d, want 200", status)
	}

	// ===== 401 without auth → HubSpot error envelope =====

	body, status = hsNoAuthGet(t, base+"/crm/v3/objects/contacts")
	if status != 401 {
		t.Fatalf("no-auth contacts -> %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if _, ok := errResp["message"].(string); !ok {
		t.Fatalf("error message = %v, want string", errResp["message"])
	}
	if _, ok := errResp["status"].(string); !ok {
		t.Fatalf("error status = %v, want string", errResp["status"])
	}
	if errResp["category"] != "AUTHENTICATION" {
		t.Fatalf("error category = %v, want AUTHENTICATION", errResp["category"])
	}

	// ===== bogus bearer token → 401 =====

	_, status = hsAuthGet(t, base+"/crm/v3/objects/contacts", "pat-bogus-token")
	if status != 401 {
		t.Fatalf("bogus-token contacts -> %d, want 401", status)
	}

	// ===== bogus hapikey → 401 =====

	_, status = hsNoAuthGet(t, base+"/crm/v3/objects/contacts?hapikey=bogus-hapikey")
	if status != 401 {
		t.Fatalf("bogus-hapikey contacts -> %d, want 401", status)
	}

	// ===== hapikey query param also works =====

	body, status = hsNoAuthGet(t, base+"/crm/v3/objects/contacts?hapikey=mock-hapikey")
	if status != 200 {
		t.Fatalf("hapikey auth -> %d, want 200; body %s", status, body)
	}
}

// TestHubspotStyleSearch exercises POST /crm/v3/objects/{objectType}/search:
// the filterGroups model (AND within a group, OR between groups), every
// supported operator, sorts, properties projection, after/limit paging, and
// the 400/401/404 failure paths.
func TestHubspotStyleSearch(t *testing.T) {
	base := hsStart(t)

	const token = "pat-mock-token"

	// Seed contacts with distinct lastnames; one carries a phone property.
	hsCreateContact := func(props map[string]any) string {
		body, status := hsAuthPostJSON(t, base+"/crm/v3/objects/contacts", token, map[string]any{"properties": props})
		if status != 201 {
			t.Fatalf("create contact -> %d, want 201; body %s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
		}
		id, _ := resp["id"].(string)
		return id
	}
	hsCreateContact(map[string]any{"firstname": "Search", "lastname": "Alpha", "email": "alpha@example.com"})
	hsCreateContact(map[string]any{"firstname": "Search", "lastname": "Bravo", "email": "bravo@example.com"})
	hsCreateContact(map[string]any{"firstname": "Search", "lastname": "Echo", "email": "echo@example.com", "phone": "+1-555-0100"})
	hsCreateContact(map[string]any{"firstname": "Search", "lastname": "Zed", "email": "zed@example.com"})

	hsSearch := func(payload map[string]any) (map[string]any, int, string) {
		body, status := hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/search", token, payload)
		if status != 200 {
			return nil, status, body
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal search resp: %v (body %s)", err, body)
		}
		return resp, status, body
	}

	// ===== EQ within one group =====

	resp, status, body := hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "lastname", "operator": "EQ", "value": "Alpha"},
			}},
		},
	})
	if status != 200 {
		t.Fatalf("search EQ -> %d, want 200; body %s", status, body)
	}
	if resp["total"] != float64(1) {
		t.Fatalf("search EQ total = %v, want 1", resp["total"])
	}
	eqResults := resp["results"].([]any)
	eq0 := eqResults[0].(map[string]any)
	if eq0["properties"].(map[string]any)["lastname"] != "Alpha" {
		t.Fatalf("search EQ lastname = %v, want Alpha", eq0["properties"])
	}

	// ===== AND within a group (two filters) =====

	resp, _, _ = hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "firstname", "operator": "EQ", "value": "Search"},
				{"propertyName": "lastname", "operator": "NEQ", "value": "Alpha"},
			}},
		},
	})
	if resp["total"] != float64(3) {
		t.Fatalf("AND group total = %v, want 3 (Bravo, Echo, Zed)", resp["total"])
	}

	// ===== OR between groups =====

	resp, _, _ = hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "lastname", "operator": "EQ", "value": "Bravo"},
			}},
			{"filters": []map[string]any{
				{"propertyName": "lastname", "operator": "EQ", "value": "Zed"},
			}},
		},
	})
	if resp["total"] != float64(2) {
		t.Fatalf("OR groups total = %v, want 2 (Bravo, Zed)", resp["total"])
	}

	// ===== IN / NOT_IN =====

	resp, _, _ = hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "email", "operator": "IN", "values": []string{"alpha@example.com", "zed@example.com"}},
			}},
		},
	})
	if resp["total"] != float64(2) {
		t.Fatalf("IN total = %v, want 2", resp["total"])
	}
	resp, _, _ = hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "lastname", "operator": "NOT_IN", "values": []string{"Alpha", "Bravo"}},
			}},
		},
	})
	// Seeded Alice (Green) plus Echo and Zed.
	if resp["total"] != float64(3) {
		t.Fatalf("NOT_IN total = %v, want 3", resp["total"])
	}

	// ===== CONTAINS_TOKEN is case-insensitive =====

	resp, _, _ = hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "email", "operator": "CONTAINS_TOKEN", "value": "@EXAMPLE.com"},
			}},
		},
	})
	if resp["total"] != float64(4) {
		t.Fatalf("CONTAINS_TOKEN total = %v, want 4", resp["total"])
	}

	// ===== HAS_PROPERTY / NOT_HAS_PROPERTY =====

	resp, _, _ = hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "phone", "operator": "HAS_PROPERTY"},
			}},
		},
	})
	if resp["total"] != float64(1) {
		t.Fatalf("HAS_PROPERTY total = %v, want 1 (Echo)", resp["total"])
	}
	resp, _, _ = hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "phone", "operator": "NOT_HAS_PROPERTY"},
			}},
		},
	})
	if resp["total"] != float64(4) {
		t.Fatalf("NOT_HAS_PROPERTY total = %v, want 4", resp["total"])
	}

	// ===== BETWEEN / GT against the seeded deal (amount "5000") =====

	dealSearch := func(payload map[string]any) map[string]any {
		body, status := hsAuthPostJSON(t, base+"/crm/v3/objects/deals/search", token, payload)
		if status != 200 {
			t.Fatalf("deal search -> %d, want 200; body %s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal deal search: %v (body %s)", err, body)
		}
		return resp
	}
	resp = dealSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "amount", "operator": "BETWEEN", "lowValue": 4000, "highValue": 6000},
			}},
		},
	})
	if resp["total"] != float64(1) {
		t.Fatalf("BETWEEN total = %v, want 1", resp["total"])
	}
	resp = dealSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "amount", "operator": "GT", "value": 6000},
			}},
		},
	})
	if resp["total"] != float64(0) {
		t.Fatalf("GT total = %v, want 0", resp["total"])
	}

	// ===== sorts + limit/after paging + properties projection =====

	page1, _, _ := hsSearch(map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "firstname", "operator": "EQ", "value": "Search"},
			}},
		},
		"sorts":      []map[string]any{{"propertyName": "lastname", "direction": "DESCENDING"}},
		"properties": []string{"email"},
		"limit":      2,
	})
	p1 := page1["results"].([]any)
	if len(p1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(p1))
	}
	if got := p1[0].(map[string]any)["properties"].(map[string]any)["lastname"]; got != nil {
		t.Fatalf("projection leaked lastname = %v, want absent", got)
	}
	// Projection keeps only the requested property names; the shape keys stay.
	if p1[0].(map[string]any)["id"] == nil {
		t.Fatalf("projection dropped id")
	}
	paging, ok := page1["paging"].(map[string]any)
	if !ok {
		t.Fatalf("paging = %v, want object (more pages exist)", page1["paging"])
	}
	after1, _ := paging["next"].(map[string]any)["after"].(string)
	if after1 != "2" {
		t.Fatalf("paging.next.after = %v, want 2", after1)
	}
	page2Payload := map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{
				{"propertyName": "firstname", "operator": "EQ", "value": "Search"},
			}},
		},
		"sorts": []map[string]any{{"propertyName": "lastname", "direction": "DESCENDING"}},
		"limit": 2,
		"after": 2,
	}
	page2, _, _ := hsSearch(page2Payload)
	if page2["paging"] != nil {
		t.Fatalf("page2 paging = %v, want null (last page)", page2["paging"])
	}

	// ===== failures =====

	// Unknown operator → 400 VALIDATION.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/search", token, map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{{"propertyName": "lastname", "operator": "FUZZY", "value": "x"}}},
		},
	})
	if status != 400 {
		t.Fatalf("unknown operator -> %d, want 400; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "VALIDATION" {
		t.Fatalf("unknown operator category = %v, want VALIDATION", cat)
	}

	// Missing propertyName → 400 VALIDATION.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/search", token, map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{{"operator": "EQ", "value": "x"}}},
		},
	})
	if status != 400 {
		t.Fatalf("missing propertyName -> %d, want 400; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "VALIDATION" {
		t.Fatalf("missing propertyName category = %v, want VALIDATION", cat)
	}

	// IN without values → 400 VALIDATION.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/search", token, map[string]any{
		"filterGroups": []map[string]any{
			{"filters": []map[string]any{{"propertyName": "email", "operator": "IN"}}},
		},
	})
	if status != 400 {
		t.Fatalf("IN without values -> %d, want 400; body %s", status, body)
	}

	// Oversized limit → 400 VALIDATION.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/search", token, map[string]any{
		"limit": 500,
	})
	if status != 400 {
		t.Fatalf("oversized limit -> %d, want 400; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "VALIDATION" {
		t.Fatalf("oversized limit category = %v, want VALIDATION", cat)
	}

	// Unparsable body → 400 VALIDATION (not an empty-dict no-op).
	body, status = hsAuthPostRaw(t, base+"/crm/v3/objects/contacts/search", token, "{not json")
	if status != 400 {
		t.Fatalf("malformed body -> %d, want 400; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "VALIDATION" {
		t.Fatalf("malformed body category = %v, want VALIDATION", cat)
	}

	// Unknown object type → 404 OBJECT_NOT_FOUND.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/widgets/search", token, map[string]any{})
	if status != 404 {
		t.Fatalf("unknown object type search -> %d, want 404; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "OBJECT_NOT_FOUND" {
		t.Fatalf("unknown object type category = %v, want OBJECT_NOT_FOUND", cat)
	}

	// No auth → 401 AUTHENTICATION.
	body, status = hsNoAuthPostJSON(t, base+"/crm/v3/objects/contacts/search", map[string]any{})
	if status != 401 {
		t.Fatalf("no-auth search -> %d, want 401; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "AUTHENTICATION" {
		t.Fatalf("no-auth search category = %v, want AUTHENTICATION", cat)
	}
}

// TestHubspotStyleBatchArchiveAssociations covers the batch endpoints for
// every object type (create/read/update/archive), the archive (soft delete)
// visibility rules on DELETE /{id}, restore, and the association DELETE +
// batch association endpoints.
func TestHubspotStyleBatchArchiveAssociations(t *testing.T) {
	base := hsStart(t)

	const token = "pat-mock-token"

	// ===== batch create companies =====

	body, status := hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/create", token, map[string]any{
		"inputs": []map[string]any{
			{"properties": map[string]any{"name": "BatchCo", "domain": "batchco.example"}},
			{"properties": map[string]any{"name": "BatchCo Two", "domain": "batchcotwo.example"}},
		},
	})
	if status != 200 {
		t.Fatalf("batch create -> %d, want 200; body %s", status, body)
	}
	var bresp map[string]any
	if err := json.Unmarshal([]byte(body), &bresp); err != nil {
		t.Fatalf("unmarshal batch create: %v (body %s)", err, body)
	}
	created, ok := bresp["results"].([]any)
	if !ok || len(created) != 2 {
		t.Fatalf("batch create results = %v, want 2", bresp["results"])
	}
	idA, _ := created[0].(map[string]any)["id"].(string)
	idB, _ := created[1].(map[string]any)["id"].(string)
	if idA == "" || idB == "" {
		t.Fatalf("batch create ids = %q %q, want non-empty", idA, idB)
	}

	// ===== batch read (properties projection, unknown ids skipped) =====

	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/read", token, map[string]any{
		"properties": []string{"domain"},
		"inputs":     []map[string]any{{"id": idA}, {"id": idB}, {"id": "999999"}},
	})
	if status != 200 {
		t.Fatalf("batch read -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &bresp); err != nil {
		t.Fatalf("unmarshal batch read: %v", err)
	}
	read, _ := bresp["results"].([]any)
	if len(read) != 2 {
		t.Fatalf("batch read results = %d, want 2 (unknown id skipped)", len(read))
	}
	readProps := read[0].(map[string]any)["properties"].(map[string]any)
	if readProps["domain"] == nil {
		t.Fatalf("batch read projection dropped domain: %v", readProps)
	}
	if readProps["name"] != nil {
		t.Fatalf("batch read projection leaked name: %v", readProps)
	}

	// ===== batch update merges =====

	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/update", token, map[string]any{
		"inputs": []map[string]any{
			{"id": idA, "properties": map[string]any{"industry": "Testing"}},
		},
	})
	if status != 200 {
		t.Fatalf("batch update -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &bresp); err != nil {
		t.Fatalf("unmarshal batch update: %v", err)
	}
	upd, _ := bresp["results"].([]any)
	updProps := upd[0].(map[string]any)["properties"].(map[string]any)
	if updProps["industry"] != "Testing" || updProps["name"] != "BatchCo" {
		t.Fatalf("batch update merge = %v, want industry=Testing and name kept", updProps)
	}

	// batch update with an unknown id → 404 OBJECT_NOT_FOUND for the call.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/update", token, map[string]any{
		"inputs": []map[string]any{
			{"id": "999999", "properties": map[string]any{"industry": "Nope"}},
		},
	})
	if status != 404 {
		t.Fatalf("batch update unknown id -> %d, want 404; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "OBJECT_NOT_FOUND" {
		t.Fatalf("batch update unknown id category = %v, want OBJECT_NOT_FOUND", cat)
	}

	// ===== batch archive (soft delete) → 204, hidden from reads =====

	_, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/archive", token, map[string]any{
		"inputs": []map[string]any{{"id": idB}},
	})
	if status != 204 {
		t.Fatalf("batch archive -> %d, want 204", status)
	}
	_, status = hsAuthGet(t, base+"/crm/v3/objects/companies/"+idB, token)
	if status != 404 {
		t.Fatalf("get archived company -> %d, want 404", status)
	}
	// Visible again with ?archived=true.
	body, status = hsAuthGet(t, base+"/crm/v3/objects/companies/"+idB+"?archived=true", token)
	if status != 200 {
		t.Fatalf("get archived company (?archived=true) -> %d, want 200; body %s", status, body)
	}
	var arch map[string]any
	if err := json.Unmarshal([]byte(body), &arch); err != nil {
		t.Fatalf("unmarshal archived company: %v", err)
	}
	if arch["archived"] != true {
		t.Fatalf("archived = %v, want true", arch["archived"])
	}
	// Default batch/read skips the archived record; archived:true reads it.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/read", token, map[string]any{
		"inputs": []map[string]any{{"id": idA}, {"id": idB}},
	})
	if status != 200 {
		t.Fatalf("batch read after archive -> %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &bresp); err != nil {
		t.Fatalf("unmarshal batch read after archive: %v", err)
	}
	if read, _ = bresp["results"].([]any); len(read) != 1 {
		t.Fatalf("batch read after archive = %d results, want 1 (archived skipped)", len(read))
	}
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/read", token, map[string]any{
		"archived": true,
		"inputs":   []map[string]any{{"id": idA}, {"id": idB}},
	})
	if err := json.Unmarshal([]byte(body), &bresp); err != nil || status != 200 {
		t.Fatalf("batch read archived:true -> %d: %v", status, err)
	}
	if read, _ = bresp["results"].([]any); len(read) != 2 {
		t.Fatalf("batch read archived:true = %d results, want 2", len(read))
	}

	// ===== restore =====

	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/"+idB+"/restore", token, map[string]any{})
	if status != 200 {
		t.Fatalf("restore -> %d, want 200; body %s", status, body)
	}
	if _, status = hsAuthGet(t, base+"/crm/v3/objects/companies/"+idB, token); status != 200 {
		t.Fatalf("get after restore -> %d, want 200", status)
	}

	// ===== DELETE /{id} archive visibility via properties=archived =====

	_, status = hsAuthDelete(t, base+"/crm/v3/objects/companies/"+idA, token)
	if status != 204 {
		t.Fatalf("delete company -> %d, want 204", status)
	}
	body, status = hsAuthGet(t, base+"/crm/v3/objects/companies?properties=archived", token)
	if status != 200 {
		t.Fatalf("list companies properties=archived -> %d, want 200; body %s", status, body)
	}
	var lresp map[string]any
	if err := json.Unmarshal([]byte(body), &lresp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	listed, _ := lresp["results"].([]any)
	foundArchived := false
	foundLive := false
	for _, r := range listed {
		rec := r.(map[string]any)
		flag := rec["properties"].(map[string]any)["archived"]
		if rec["id"] == idA {
			if flag != "true" {
				t.Fatalf("archived record flag = %v, want \"true\"", flag)
			}
			foundArchived = true
		} else if flag == "false" {
			foundLive = true
		}
	}
	if !foundArchived || !foundLive {
		t.Fatalf("properties=archived list must include archived and live records; archived=%v live=%v", foundArchived, foundLive)
	}
	// Default list hides the archived record.
	body, status = hsAuthGet(t, base+"/crm/v3/objects/companies", token)
	if status != 200 {
		t.Fatalf("default list -> %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &lresp); err != nil {
		t.Fatalf("unmarshal default list: %v", err)
	}
	listed, _ = lresp["results"].([]any)
	for _, r := range listed {
		if r.(map[string]any)["id"] == idA {
			t.Fatalf("default list must not include archived company %s", idA)
		}
	}

	// ===== batch validation failures =====

	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/create", token, map[string]any{
		"inputs": []map[string]any{},
	})
	if status != 400 {
		t.Fatalf("batch create empty inputs -> %d, want 400; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "VALIDATION" {
		t.Fatalf("batch create empty inputs category = %v, want VALIDATION", cat)
	}
	tooMany := make([]map[string]any, 101)
	for i := range tooMany {
		tooMany[i] = map[string]any{"properties": map[string]any{"name": "X"}}
	}
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/companies/batch/create", token, map[string]any{
		"inputs": tooMany,
	})
	if status != 400 {
		t.Fatalf("batch create 101 inputs -> %d, want 400", status)
	}
	body, status = hsAuthPostRaw(t, base+"/crm/v3/objects/companies/batch/read", token, "][")
	if status != 400 {
		t.Fatalf("batch read malformed body -> %d, want 400; body %s", status, body)
	}
	_, status = hsAuthPostJSON(t, base+"/crm/v3/objects/widgets/batch/read", token, map[string]any{
		"inputs": []map[string]any{{"id": "1"}},
	})
	if status != 404 {
		t.Fatalf("batch read unknown object type -> %d, want 404", status)
	}

	// ===== associations: batch create / batch archive / DELETE =====

	hsCreateContact := func(first string) string {
		body, status := hsAuthPostJSON(t, base+"/crm/v3/objects/contacts", token, map[string]any{
			"properties": map[string]any{"firstname": first, "lastname": "Batch"},
		})
		if status != 201 {
			t.Fatalf("create contact -> %d; body %s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal create contact: %v", err)
		}
		id, _ := resp["id"].(string)
		return id
	}
	c1 := hsCreateContact("BatchOne")
	c2 := hsCreateContact("BatchTwo")
	coID := "1" // seeded Acme Corp

	listAssocs := func(contact string) []any {
		body, status := hsAuthGet(t, base+"/crm/v3/objects/contacts/"+contact+"/associations/company", token)
		if status != 200 {
			t.Fatalf("list associations -> %d; body %s", status, body)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("unmarshal associations: %v", err)
		}
		out, _ := resp["results"].([]any)
		return out
	}

	// Batch associate both contacts with the seeded company.
	_, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/"+c1+"/associations/company/batch/create", token, map[string]any{
		"inputs": []map[string]any{
			{"id": coID, "associationType": "contact_to_company"},
			{"id": coID, "associationType": "contact_to_company_primary"},
		},
	})
	if status != 204 {
		t.Fatalf("batch associate -> %d, want 204", status)
	}
	if got := listAssocs(c1); len(got) != 2 {
		t.Fatalf("associations after batch create = %d, want 2", len(got))
	}

	// Batch archive one of them.
	_, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/"+c1+"/associations/company/batch/archive", token, map[string]any{
		"inputs": []map[string]any{{"id": coID, "associationType": "contact_to_company_primary"}},
	})
	if status != 204 {
		t.Fatalf("batch disassociate -> %d, want 204", status)
	}
	if got := listAssocs(c1); len(got) != 1 {
		t.Fatalf("associations after batch archive = %d, want 1", len(got))
	}

	// DELETE the remaining association; a second DELETE 404s.
	_, status = hsAuthDelete(t, base+"/crm/v3/objects/contacts/"+c1+
		"/associations/company/"+coID+"/contact_to_company", token)
	if status != 204 {
		t.Fatalf("delete association -> %d, want 204", status)
	}
	if got := listAssocs(c1); len(got) != 0 {
		t.Fatalf("associations after delete = %d, want 0", len(got))
	}
	_, status = hsAuthDelete(t, base+"/crm/v3/objects/contacts/"+c1+
		"/associations/company/"+coID+"/contact_to_company", token)
	if status != 404 {
		t.Fatalf("delete missing association -> %d, want 404", status)
	}

	// Batch associate for a second contact (different from-record, same
	// collection) and clean up via the generic objectType route.
	_, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/"+c2+"/associations/company/batch/create", token, map[string]any{
		"inputs": []map[string]any{{"id": coID, "associationType": "contact_to_company"}},
	})
	if status != 204 {
		t.Fatalf("batch associate (second contact) -> %d, want 204", status)
	}
	if got := listAssocs(c2); len(got) != 1 {
		t.Fatalf("second contact associations = %d, want 1", len(got))
	}

	// Validation: missing inputs → 400.
	body, status = hsAuthPostJSON(t, base+"/crm/v3/objects/contacts/"+c1+"/associations/company/batch/create", token, map[string]any{})
	if status != 400 {
		t.Fatalf("batch associate without inputs -> %d, want 400; body %s", status, body)
	}
	if cat := hsCategory(t, body); cat != "VALIDATION" {
		t.Fatalf("batch associate without inputs category = %v, want VALIDATION", cat)
	}
}

// === HubSpot test helpers ===

// hsStart boots the hubspot-style adapter on an ephemeral port and returns
// its base URL.
func hsStart(t *testing.T) string {
	t.Helper()
	adapterDir := filepath.Join("..", "..", "adapters", "hubspot-style")
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
			"hubspot": {Adapter: absAdapterDir},
		},
	}
	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(50 * time.Millisecond)
	return addrs["hubspot"]
}

// hsCategory extracts the HubSpot error category from an error envelope.
func hsCategory(t *testing.T, body string) string {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body %s)", err, body)
	}
	cat, _ := resp["category"].(string)
	return cat
}

func hsAuthGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return hsDo(t, req)
}

func hsNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	return hsDo(t, req)
}

func hsAuthPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	return hsPostJSON(t, rawurl, token, payload)
}

func hsNoAuthPostJSON(t *testing.T, rawurl string, payload map[string]any) (string, int) {
	t.Helper()
	return hsPostJSON(t, rawurl, "", payload)
}

func hsPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return hsDo(t, req)
}

// hsAuthPostRaw posts a verbatim (possibly malformed) body — used to prove
// undecodable bodies surface as 400s, not empty-dict no-ops.
func hsAuthPostRaw(t *testing.T, rawurl, token, raw string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return hsDo(t, req)
}

func hsAuthPatchJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return hsDo(t, req)
}

func hsAuthPut(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("PUT", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	return hsDo(t, req)
}

func hsAuthDelete(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return hsDo(t, req)
}

func hsDo(t *testing.T, req *http.Request) (string, int) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
