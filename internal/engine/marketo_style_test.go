package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestMarketoStyleAdapter exercises the Marketo Engage REST adapter end-to-end:
//
//   - mint access token via OAuth client_credentials
//   - list leads (filter by email)
//   - create lead
//   - get lead by id
//   - list campaigns
//   - trigger campaign for leads
//   - get paging token for activities
//   - 401 without auth → Marketo {success:false, errors} envelope
func TestMarketoStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "marketo-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	manifestPath := filepath.Join(stateDir, "stunt.yaml")

	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"marketo": {Adapter: absAdapterDir},
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

	base := addrs["marketo"]

	// ===== mint access token (OAuth client_credentials) =====

	body, status := marketoGet(t, base+"/identity/oauth/token"+
		"?grant_type=client_credentials&client_id=test-id&client_secret=test-secret")
	if status != 200 {
		t.Fatalf("token mint -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token resp: %v (body %s)", err, body)
	}
	accessToken, ok := tokenResp["access_token"].(string)
	if !ok || accessToken == "" {
		t.Fatalf("access_token = %v, want non-empty string", tokenResp["access_token"])
	}
	if tokenResp["token_type"] != "bearer" {
		t.Fatalf("token_type = %v, want bearer", tokenResp["token_type"])
	}
	if tokenResp["expires_in"] != float64(3600) {
		t.Fatalf("expires_in = %v, want 3600", tokenResp["expires_in"])
	}

	// ===== list leads (no filter, verify seeded data + shape) =====

	body, status = marketoAuthGet(t, base+"/rest/v1/leads", accessToken)
	if status != 200 {
		t.Fatalf("list leads -> %d, want 200; body %s", status, body)
	}
	var listResp map[string]any
	if err := json.Unmarshal([]byte(body), &listResp); err != nil {
		t.Fatalf("unmarshal list resp: %v (body %s)", err, body)
	}
	if listResp["success"] != true {
		t.Fatalf("success = %v, want true", listResp["success"])
	}
	if _, ok := listResp["requestId"].(string); !ok {
		t.Fatalf("requestId = %v, want string", listResp["requestId"])
	}
	result, ok := listResp["result"].([]any)
	if !ok || len(result) == 0 {
		t.Fatalf("result = %v, want non-empty", listResp["result"])
	}
	lead0 := result[0].(map[string]any)
	if _, ok := lead0["email"].(string); !ok {
		t.Fatalf("lead email = %v, want string", lead0["email"])
	}
	if _, ok := lead0["id"].(string); !ok {
		t.Fatalf("lead id = %v, want string", lead0["id"])
	}

	// ===== create lead =====

	createEmail := "test.user@example.com"
	body, status = marketoAuthPostJSON(t, base+"/rest/v1/leads", accessToken, map[string]any{
		"action":    "createOnly",
		"firstName": "Test",
		"lastName":  "User",
		"email":     createEmail,
	})
	if status != 200 {
		t.Fatalf("create lead -> %d, want 200; body %s", status, body)
	}
	var createResp map[string]any
	if err := json.Unmarshal([]byte(body), &createResp); err != nil {
		t.Fatalf("unmarshal create resp: %v (body %s)", err, body)
	}
	if createResp["success"] != true {
		t.Fatalf("create success = %v, want true", createResp["success"])
	}
	createResult, ok := createResp["result"].([]any)
	if !ok || len(createResult) != 1 {
		t.Fatalf("create result = %v, want 1", createResp["result"])
	}
	createdLead := createResult[0].(map[string]any)
	newLeadID, ok := createdLead["id"].(string)
	if !ok || newLeadID == "" {
		t.Fatalf("created lead id = %v, want non-empty string", createdLead["id"])
	}
	if createdLead["status"] != "created" {
		t.Fatalf("created lead status = %v, want created", createdLead["status"])
	}

	// ===== list leads (filter by email — the created lead) =====

	body, status = marketoAuthGet(t, base+"/rest/v1/leads?filterType=email&filterValues="+url.QueryEscape(createEmail), accessToken)
	if status != 200 {
		t.Fatalf("list leads (filter) -> %d, want 200; body %s", status, body)
	}
	var filterResp map[string]any
	if err := json.Unmarshal([]byte(body), &filterResp); err != nil {
		t.Fatalf("unmarshal filter resp: %v (body %s)", err, body)
	}
	filterResult, ok := filterResp["result"].([]any)
	if !ok || len(filterResult) != 1 {
		t.Fatalf("filter result = %v, want exactly 1 lead", filterResp["result"])
	}
	if filterResult[0].(map[string]any)["email"] != createEmail {
		t.Fatalf("filtered lead email mismatch")
	}

	// ===== get lead by id =====

	body, status = marketoAuthGet(t, base+"/rest/v1/leads/"+newLeadID, accessToken)
	if status != 200 {
		t.Fatalf("get lead -> %d, want 200; body %s", status, body)
	}
	var getResp map[string]any
	if err := json.Unmarshal([]byte(body), &getResp); err != nil {
		t.Fatalf("unmarshal get lead: %v (body %s)", err, body)
	}
	if getResp["success"] != true {
		t.Fatalf("get lead success = %v, want true", getResp["success"])
	}
	getResult, ok := getResp["result"].([]any)
	if !ok || len(getResult) != 1 {
		t.Fatalf("get result = %v, want 1", getResp["result"])
	}
	if getResult[0].(map[string]any)["id"] != newLeadID {
		t.Fatalf("retrieved lead id mismatch")
	}

	// ===== list campaigns =====

	body, status = marketoAuthGet(t, base+"/rest/v1/campaigns", accessToken)
	if status != 200 {
		t.Fatalf("list campaigns -> %d, want 200; body %s", status, body)
	}
	var campResp map[string]any
	if err := json.Unmarshal([]byte(body), &campResp); err != nil {
		t.Fatalf("unmarshal campaigns: %v (body %s)", err, body)
	}
	if campResp["success"] != true {
		t.Fatalf("campaigns success = %v, want true", campResp["success"])
	}
	campResult, ok := campResp["result"].([]any)
	if !ok || len(campResult) == 0 {
		t.Fatalf("campaigns result = %v, want non-empty", campResp["result"])
	}
	campaign0 := campResult[0].(map[string]any)
	campaignID, _ := campaign0["id"].(string)
	if campaignID == "" {
		t.Fatalf("campaign id = %v", campaign0["id"])
	}

	// ===== trigger campaign for leads =====

	body, status = marketoAuthPostJSON(t, base+"/rest/v1/campaigns/"+campaignID+"/trigger", accessToken, map[string]any{
		"input": []map[string]any{
			{"leadId": newLeadID},
		},
	})
	if status != 200 {
		t.Fatalf("trigger campaign -> %d, want 200; body %s", status, body)
	}
	var trigResp map[string]any
	if err := json.Unmarshal([]byte(body), &trigResp); err != nil {
		t.Fatalf("unmarshal trigger: %v (body %s)", err, body)
	}
	if trigResp["success"] != true {
		t.Fatalf("trigger success = %v, want true", trigResp["success"])
	}

	// ===== paging token for activities =====

	body, status = marketoAuthGet(t, base+"/rest/v1/activities/pagingtoken?sinceDatetime=2024-01-01T00:00:00Z", accessToken)
	if status != 200 {
		t.Fatalf("paging token -> %d, want 200; body %s", status, body)
	}
	var ptResp map[string]any
	if err := json.Unmarshal([]byte(body), &ptResp); err != nil {
		t.Fatalf("unmarshal paging token: %v (body %s)", err, body)
	}
	if ptResp["success"] != true {
		t.Fatalf("paging token success = %v, want true", ptResp["success"])
	}
	pageToken, ok := ptResp["nextPageToken"].(string)
	if !ok || pageToken == "" {
		t.Fatalf("nextPageToken = %v, want non-empty string", ptResp["nextPageToken"])
	}

	// ===== list activities =====

	body, status = marketoAuthGet(t, base+"/rest/v1/activities?activityTypeIds=12,13", accessToken)
	if status != 200 {
		t.Fatalf("list activities -> %d, want 200; body %s", status, body)
	}
	var actResp map[string]any
	if err := json.Unmarshal([]byte(body), &actResp); err != nil {
		t.Fatalf("unmarshal activities: %v (body %s)", err, body)
	}
	if actResp["success"] != true {
		t.Fatalf("activities success = %v, want true", actResp["success"])
	}

	// ===== sync leads: createOrUpdate creates, with CUSTOM fields =====

	syncEmail := "custom.fields@example.com"
	body, status = marketoAuthPostJSON(t, base+"/rest/v1/leads.json", accessToken, map[string]any{
		"action": "createOrUpdate",
		"input": []map[string]any{
			{
				"email":     syncEmail,
				"firstName": "Custom",
				"lastName":  "Fields",
				"company":   "Acme Corp",
				"leadScore": 42,
			},
		},
	})
	if status != 200 {
		t.Fatalf("sync create -> %d, want 200; body %s", status, body)
	}
	var syncResp map[string]any
	if err := json.Unmarshal([]byte(body), &syncResp); err != nil {
		t.Fatalf("unmarshal sync: %v (body %s)", err, body)
	}
	syncResult, ok := syncResp["result"].([]any)
	if !ok || len(syncResult) != 1 {
		t.Fatalf("sync result = %v, want 1", syncResp["result"])
	}
	if syncResult[0].(map[string]any)["status"] != "created" {
		t.Fatalf("sync status = %v, want created", syncResult[0].(map[string]any)["status"])
	}

	// ===== sync leads again: updateOnly-less update preserves custom fields =====

	body, status = marketoAuthPostJSON(t, base+"/rest/v1/leads.json", accessToken, map[string]any{
		"action": "createOrUpdate",
		"input": []map[string]any{
			{"email": syncEmail, "firstName": "Renamed"},
		},
	})
	if status != 200 {
		t.Fatalf("sync update -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &syncResp); err != nil {
		t.Fatalf("unmarshal sync update: %v (body %s)", err, body)
	}
	syncResult, ok = syncResp["result"].([]any)
	if !ok || len(syncResult) != 1 {
		t.Fatalf("sync update result = %v, want 1", syncResp["result"])
	}
	if syncResult[0].(map[string]any)["status"] != "updated" {
		t.Fatalf("sync update status = %v, want updated", syncResult[0].(map[string]any)["status"])
	}

	// Verify custom fields survived the update (currently dropped pre-fix).
	body, status = marketoAuthGet(t, base+"/rest/v1/leads?filterType=email&filterValues="+url.QueryEscape(syncEmail), accessToken)
	if status != 200 {
		t.Fatalf("list custom-field lead -> %d, want 200; body %s", status, body)
	}
	var customResp map[string]any
	if err := json.Unmarshal([]byte(body), &customResp); err != nil {
		t.Fatalf("unmarshal custom-field lead: %v (body %s)", err, body)
	}
	customResult, ok := customResp["result"].([]any)
	if !ok || len(customResult) != 1 {
		t.Fatalf("custom-field result = %v, want 1", customResp["result"])
	}
	customLead := customResult[0].(map[string]any)
	if customLead["company"] != "Acme Corp" {
		t.Fatalf("custom field company = %v, want Acme Corp (preserved through update)", customLead["company"])
	}
	if customLead["leadScore"] != float64(42) {
		t.Fatalf("custom field leadScore = %v, want 42 (preserved through update)", customLead["leadScore"])
	}
	if customLead["firstName"] != "Renamed" {
		t.Fatalf("firstName = %v, want Renamed", customLead["firstName"])
	}

	// ===== updateOnly on a nonexistent lead -> skipped, NOT created =====

	body, status = marketoAuthPostJSON(t, base+"/rest/v1/leads.json", accessToken, map[string]any{
		"action": "updateOnly",
		"input": []map[string]any{
			{"email": "ghost.lead@example.com", "firstName": "Ghost"},
		},
	})
	if status != 200 {
		t.Fatalf("sync updateOnly nonexistent -> %d, want 200; body %s", status, body)
	}
	var skipResp map[string]any
	if err := json.Unmarshal([]byte(body), &skipResp); err != nil {
		t.Fatalf("unmarshal skip resp: %v (body %s)", err, body)
	}
	skipResult, ok := skipResp["result"].([]any)
	if !ok || len(skipResult) != 1 {
		t.Fatalf("skip result = %v, want 1", skipResp["result"])
	}
	skipLead := skipResult[0].(map[string]any)
	if skipLead["status"] != "skipped" {
		t.Fatalf("updateOnly nonexistent status = %v, want skipped", skipLead["status"])
	}
	reasons, ok := skipLead["reasons"].([]any)
	if !ok || len(reasons) != 1 {
		t.Fatalf("skip reasons = %v, want 1", skipLead["reasons"])
	}
	if reasons[0].(map[string]any)["code"] != "1013" {
		t.Fatalf("skip reason code = %v, want 1013", reasons[0].(map[string]any)["code"])
	}

	// Verify the skipped lead was never created.
	body, status = marketoAuthGet(t, base+"/rest/v1/leads?filterType=email&filterValues="+url.QueryEscape("ghost.lead@example.com"), accessToken)
	if status != 200 {
		t.Fatalf("list ghost lead -> %d, want 200; body %s", status, body)
	}
	var ghostResp map[string]any
	if err := json.Unmarshal([]byte(body), &ghostResp); err != nil {
		t.Fatalf("unmarshal ghost resp: %v (body %s)", err, body)
	}
	if ghostList, ok := ghostResp["result"].([]any); !ok || len(ghostList) != 0 {
		t.Fatalf("ghost lead result = %v, want empty (skipped must not create)", ghostResp["result"])
	}

	// ===== fields= projection on leads list =====

	body, status = marketoAuthGet(t, base+"/rest/v1/leads?filterType=email&filterValues="+url.QueryEscape(syncEmail)+"&fields=id,email", accessToken)
	if status != 200 {
		t.Fatalf("list projected -> %d, want 200; body %s", status, body)
	}
	var projResp map[string]any
	if err := json.Unmarshal([]byte(body), &projResp); err != nil {
		t.Fatalf("unmarshal projected: %v (body %s)", err, body)
	}
	projResult, ok := projResp["result"].([]any)
	if !ok || len(projResult) != 1 {
		t.Fatalf("projected result = %v, want 1", projResp["result"])
	}
	projLead := projResult[0].(map[string]any)
	if projLead["email"] != syncEmail {
		t.Fatalf("projected email = %v, want %v", projLead["email"], syncEmail)
	}
	if _, ok := projLead["id"]; !ok {
		t.Fatalf("projected lead missing id (always included)")
	}
	if _, ok := projLead["firstName"]; ok {
		t.Fatalf("projected lead has firstName = %v, want it projected out", projLead["firstName"])
	}

	// ===== leads describe: field metadata shape =====

	body, status = marketoAuthGet(t, base+"/rest/v1/leads/describe", accessToken)
	if status != 200 {
		t.Fatalf("leads describe -> %d, want 200; body %s", status, body)
	}
	var descResp map[string]any
	if err := json.Unmarshal([]byte(body), &descResp); err != nil {
		t.Fatalf("unmarshal describe: %v (body %s)", err, body)
	}
	if descResp["success"] != true {
		t.Fatalf("describe success = %v, want true", descResp["success"])
	}
	descResult, ok := descResp["result"].([]any)
	if !ok || len(descResult) == 0 {
		t.Fatalf("describe result = %v, want non-empty", descResp["result"])
	}
	foundEmailField := false
	for _, f := range descResult {
		fm := f.(map[string]any)
		if fm["name"] != "email" {
			continue
		}
		foundEmailField = true
		if fm["dataType"] != "email" {
			t.Fatalf("email dataType = %v, want email", fm["dataType"])
		}
		rest, ok := fm["rest"].(map[string]any)
		if !ok || rest["name"] != "email" {
			t.Fatalf("email rest binding = %v, want rest.name email", fm["rest"])
		}
		if _, ok := fm["displayName"].(string); !ok {
			t.Fatalf("email displayName = %v, want string", fm["displayName"])
		}
	}
	if !foundEmailField {
		t.Fatalf("describe result has no email field metadata")
	}

	// ===== bulk lead extract: create job -> Queued =====

	body, status = marketoAuthPostJSON(t, base+"/bulk/v1/leads/export/create", accessToken, map[string]any{
		"fields": []string{"id", "email", "firstName"},
	})
	if status != 200 {
		t.Fatalf("export create -> %d, want 200; body %s", status, body)
	}
	var expCreate map[string]any
	if err := json.Unmarshal([]byte(body), &expCreate); err != nil {
		t.Fatalf("unmarshal export create: %v (body %s)", err, body)
	}
	if expCreate["success"] != true {
		t.Fatalf("export create success = %v, want true", expCreate["success"])
	}
	expResult, ok := expCreate["result"].([]any)
	if !ok || len(expResult) != 1 {
		t.Fatalf("export create result = %v, want 1", expCreate["result"])
	}
	exportID, ok := expResult[0].(map[string]any)["exportId"].(string)
	if !ok || exportID == "" {
		t.Fatalf("exportId = %v, want non-empty string", expResult[0])
	}
	if expResult[0].(map[string]any)["status"] != "Queued" {
		t.Fatalf("export status = %v, want Queued", expResult[0].(map[string]any)["status"])
	}

	// ===== bulk extract: file before completion -> 400 code 1030 =====

	body, status = marketoAuthGet(t, base+"/bulk/v1/leads/export/"+exportID+"/file", accessToken)
	if status != 400 {
		t.Fatalf("export file (incomplete) -> %d, want 400; body %s", status, body)
	}
	var inFileResp map[string]any
	if err := json.Unmarshal([]byte(body), &inFileResp); err != nil {
		t.Fatalf("unmarshal incomplete file resp: %v (body %s)", err, body)
	}
	if inFileResp["success"] != false {
		t.Fatalf("incomplete file success = %v, want false", inFileResp["success"])
	}
	inErrs, ok := inFileResp["errors"].([]any)
	if !ok || len(inErrs) != 1 || inErrs[0].(map[string]any)["code"] != "1030" {
		t.Fatalf("incomplete file errors = %v, want code 1030", inFileResp["errors"])
	}

	// ===== bulk extract: poll -> Processing (derive-on-read) =====

	time.Sleep(1500 * time.Millisecond)
	body, status = marketoAuthGet(t, base+"/bulk/v1/leads/export/"+exportID+"/status", accessToken)
	if status != 200 {
		t.Fatalf("export status poll -> %d, want 200; body %s", status, body)
	}
	var pollResp map[string]any
	if err := json.Unmarshal([]byte(body), &pollResp); err != nil {
		t.Fatalf("unmarshal poll: %v (body %s)", err, body)
	}
	pollResult, ok := pollResp["result"].([]any)
	if !ok || len(pollResult) != 1 {
		t.Fatalf("poll result = %v, want 1", pollResp["result"])
	}
	if pollResult[0].(map[string]any)["status"] != "Processing" {
		t.Fatalf("poll status = %v, want Processing", pollResult[0])
	}

	// ===== bulk extract: poll -> Completed with fileSize/checksum; download CSV =====

	time.Sleep(2100 * time.Millisecond)
	body, status = marketoAuthGet(t, base+"/bulk/v1/leads/export/"+exportID+"/status", accessToken)
	if status != 200 {
		t.Fatalf("export status poll 2 -> %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &pollResp); err != nil {
		t.Fatalf("unmarshal poll 2: %v (body %s)", err, body)
	}
	pollResult, ok = pollResp["result"].([]any)
	if !ok || len(pollResult) != 1 {
		t.Fatalf("poll 2 result = %v, want 1", pollResp["result"])
	}
	doneItem := pollResult[0].(map[string]any)
	if doneItem["status"] != "Completed" {
		t.Fatalf("poll 2 status = %v, want Completed", doneItem["status"])
	}
	if _, ok := doneItem["fileSize"]; !ok {
		t.Fatalf("completed poll missing fileSize")
	}
	if _, ok := doneItem["fileChecksum"]; !ok {
		t.Fatalf("completed poll missing fileChecksum")
	}

	body, status = marketoAuthGet(t, base+"/bulk/v1/leads/export/"+exportID+"/file", accessToken)
	if status != 200 {
		t.Fatalf("export file -> %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "id,email,firstName") {
		t.Fatalf("export CSV header mismatch: %q", body)
	}
	if !strings.Contains(body, syncEmail) {
		t.Fatalf("export CSV missing synced lead %s: %q", syncEmail, body)
	}

	// ===== bulk extract: cancelling a Completed job fails =====

	body, status = marketoAuthPostJSON(t, base+"/bulk/v1/leads/export/"+exportID+"/cancel", accessToken, map[string]any{})
	if status != 400 {
		t.Fatalf("cancel completed -> %d, want 400; body %s", status, body)
	}

	// ===== bulk extract: cancel a fresh job -> Cancelled =====

	body, status = marketoAuthPostJSON(t, base+"/bulk/v1/leads/export/create", accessToken, map[string]any{})
	if status != 200 {
		t.Fatalf("export create 2 -> %d, want 200; body %s", status, body)
	}
	var expCreate2 map[string]any
	if err := json.Unmarshal([]byte(body), &expCreate2); err != nil {
		t.Fatalf("unmarshal export create 2: %v (body %s)", err, body)
	}
	exp2Result, ok := expCreate2["result"].([]any)
	if !ok || len(exp2Result) != 1 {
		t.Fatalf("export create 2 result = %v, want 1", expCreate2["result"])
	}
	exportID2, _ := exp2Result[0].(map[string]any)["exportId"].(string)

	body, status = marketoAuthPostJSON(t, base+"/bulk/v1/leads/export/"+exportID2+"/cancel", accessToken, map[string]any{})
	if status != 200 {
		t.Fatalf("export cancel -> %d, want 200; body %s", status, body)
	}
	var cancelResp map[string]any
	if err := json.Unmarshal([]byte(body), &cancelResp); err != nil {
		t.Fatalf("unmarshal cancel: %v (body %s)", err, body)
	}
	cancelResult, ok := cancelResp["result"].([]any)
	if !ok || len(cancelResult) != 1 {
		t.Fatalf("cancel result = %v, want 1", cancelResp["result"])
	}
	if cancelResult[0].(map[string]any)["status"] != "Cancelled" {
		t.Fatalf("cancel status = %v, want Cancelled", cancelResult[0])
	}

	// ===== bulk extract: unknown job -> 404 =====

	body, status = marketoAuthGet(t, base+"/bulk/v1/leads/export/does-not-exist/status", accessToken)
	if status != 404 {
		t.Fatalf("unknown export poll -> %d, want 404; body %s", status, body)
	}

	// ===== 401 without auth → Marketo error envelope =====

	body, status = marketoNoAuthGet(t, base+"/rest/v1/leads")
	if status != 401 {
		t.Fatalf("no-auth leads -> %d, want 401; body %s", status, body)
	}
	var errResp map[string]any
	if err := json.Unmarshal([]byte(body), &errResp); err != nil {
		t.Fatalf("unmarshal error resp: %v (body %s)", err, body)
	}
	if errResp["success"] != false {
		t.Fatalf("error success = %v, want false", errResp["success"])
	}
	errors, ok := errResp["errors"].([]any)
	if !ok || len(errors) == 0 {
		t.Fatalf("errors = %v, want non-empty array", errResp["errors"])
	}
	err0 := errors[0].(map[string]any)
	if _, ok := err0["code"].(string); !ok {
		t.Fatalf("error code = %v, want string", err0["code"])
	}
	if _, ok := err0["message"].(string); !ok {
		t.Fatalf("error message = %v, want string", err0["message"])
	}

	// ===== bogus token → 401 =====

	_, status = marketoAuthGet(t, base+"/rest/v1/leads", "synthetic_token_bogus")
	if status != 401 {
		t.Fatalf("bogus-token leads -> %d, want 401", status)
	}

	// ===== access_token query param also works =====

	body, status = marketoNoAuthGet(t, base+"/rest/v1/campaigns?access_token="+accessToken)
	if status != 200 {
		t.Fatalf("access_token query auth -> %d, want 200; body %s", status, body)
	}
}

// === Marketo test helpers ===

func marketoAuthGet(t *testing.T, rawurl, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func marketoGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func marketoNoAuthGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func marketoAuthPostJSON(t *testing.T, rawurl, token string, payload map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", rawurl, strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// Guard: suppress unused imports.
var _ = fmt.Sprintf
var _ = url.QueryEscape

// TestMarketoStyleLiveTimestamps verifies lead createdAt/updatedAt are
// live clock timestamps (RFC 3339) rather than a fixed synthetic date.
func TestMarketoStyleLiveTimestamps(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "marketo-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"marketo": {Adapter: adapterDir},
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
	base := addrs["marketo"]

	body, status := marketoGet(t, base+"/identity/oauth/token"+
		"?grant_type=client_credentials&client_id=test-id&client_secret=test-secret")
	if status != 200 {
		t.Fatalf("token mint -> %d, want 200; body %s", status, body)
	}
	var tokenResp map[string]any
	if err := json.Unmarshal([]byte(body), &tokenResp); err != nil {
		t.Fatalf("unmarshal token: %v (body %s)", err, body)
	}
	accessToken := tokenResp["access_token"].(string)

	start := time.Now().UTC()
	body, status = marketoAuthPostJSON(t, base+"/rest/v1/leads.json", accessToken, map[string]any{
		"action": "createOnly",
		"input": []map[string]any{
			{"email": "clock-test@example.com", "firstName": "Clock", "lastName": "Test"},
		},
	})
	if status != 200 {
		t.Fatalf("sync lead -> %d, want 200; body %s", status, body)
	}
	var syncResp map[string]any
	if err := json.Unmarshal([]byte(body), &syncResp); err != nil {
		t.Fatalf("unmarshal sync: %v (body %s)", err, body)
	}
	syncResult, ok := syncResp["result"].([]any)
	if !ok || len(syncResult) != 1 {
		t.Fatalf("sync result = %v, want 1 entry", syncResp["result"])
	}
	leadID, _ := syncResult[0].(map[string]any)["id"].(string)
	if leadID == "" {
		t.Fatalf("lead id = %v, want non-empty", syncResult[0])
	}

	body, status = marketoAuthGet(t, base+"/rest/v1/leads/"+leadID+".json", accessToken)
	if status != 200 {
		t.Fatalf("get lead -> %d, want 200; body %s", status, body)
	}
	var leadResp map[string]any
	if err := json.Unmarshal([]byte(body), &leadResp); err != nil {
		t.Fatalf("unmarshal lead: %v (body %s)", err, body)
	}
	leadResult, ok := leadResp["result"].([]any)
	if !ok || len(leadResult) != 1 {
		t.Fatalf("lead result = %v, want 1 entry", leadResp["result"])
	}
	lead := leadResult[0].(map[string]any)
	for _, field := range []string{"createdAt", "updatedAt"} {
		raw, _ := lead[field].(string)
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t.Fatalf("lead %s = %q is not RFC 3339: %v", field, raw, err)
		}
		if ts.Before(start.Add(-time.Minute)) || ts.After(time.Now().Add(time.Minute)) {
			t.Fatalf("lead %s = %v not live (start %v)", field, ts, start)
		}
	}
}
