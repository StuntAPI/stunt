package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestAzureDevOpsStyleAdapter exercises the azure-devops-style adapter:
//
//   - PAT auth required: 401 without auth
//   - List projects → {value, count}
//   - List git repos for a project
//   - Create work item (PATCH-style body) → retrievable by id
//   - Get work item by id → seeded item
//   - Iterations
func TestAzureDevOpsStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "azure-devops-style")
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
			"ado": {Adapter: absAdapterDir},
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

	base := addrs["ado"]

	// PAT as Basic auth (PAT:emptyPassword)
	patBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("testPAT:"))

	// ===== 401 without auth =====

	body, status := adoGet(t, base+"/myorg/_apis/projects", "")
	if status != 401 {
		t.Fatalf("projects without auth -> status %d, want 401; body %s", status, body)
	}

	// ===== 401 with an unknown (never-issued) PAT =====

	body, status = adoGet(t, base+"/myorg/_apis/projects", "Bearer bogusPAT")
	if status != 401 {
		t.Fatalf("projects with unknown PAT -> status %d, want 401; body %s", status, body)
	}

	// ===== List projects =====

	body, status = adoGet(t, base+"/myorg/_apis/projects", patBasic)
	if status != 200 {
		t.Fatalf("projects -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	value, ok := resp["value"].([]any)
	if !ok || len(value) < 1 {
		t.Fatalf("value = %v, want non-empty array", resp["value"])
	}
	first := value[0].(map[string]any)
	if _, ok := first["id"].(string); !ok {
		t.Fatalf("project id = %v, want string", first["id"])
	}
	if _, ok := first["name"].(string); !ok {
		t.Fatalf("project name = %v, want string", first["name"])
	}
	if resp["count"] != float64(len(value)) {
		t.Fatalf("count = %v, want %d", resp["count"], len(value))
	}

	// ===== List git repos =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/git/repositories", patBasic)
	if status != 200 {
		t.Fatalf("repos -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal repos: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 1 {
		t.Fatalf("repos count = %d, want >= 1", len(value))
	}

	// ===== Create work item (typed route + PATCH-style body) =====

	createBody := []map[string]any{
		{"op": "add", "path": "/fields/System.Title", "value": "New task from API"},
		{"op": "add", "path": "/fields/System.Description", "value": "Created via stunt test"},
	}
	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/wit/workitems/Task", patBasic, createBody)
	if status != 200 {
		t.Fatalf("create work item -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal created wi: %v (body %s)", err, body)
	}
	newID, ok := resp["id"].(float64)
	if !ok {
		t.Fatalf("created wi id = %v, want number", resp["id"])
	}
	fields, ok := resp["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %v, want object", resp["fields"])
	}
	if fields["System.Title"] != "New task from API" {
		t.Fatalf("System.Title = %v, want 'New task from API'", fields["System.Title"])
	}
	if fields["System.WorkItemType"] != "Task" {
		t.Fatalf("System.WorkItemType = %v, want Task (from the typed route)", fields["System.WorkItemType"])
	}

	// ===== Get work item by id (seeded) =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/wit/workitems/1", patBasic)
	if status != 200 {
		t.Fatalf("get work item -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal wi: %v (body %s)", err, body)
	}
	if resp["id"] != float64(1) {
		t.Fatalf("wi id = %v, want 1", resp["id"])
	}

	// ===== Get created work item by id (STATEFUL) =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/wit/workitems/"+strconv.Itoa(int(newID)), patBasic)
	if status != 200 {
		t.Fatalf("get created wi -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if resp["id"] != newID {
		t.Fatalf("created wi id = %v, want %v", resp["id"], newID)
	}

	// ===== Update work item via PATCH (patch-document semantics) =====

	patchBody := []map[string]any{
		{"op": "replace", "path": "/fields/System.State", "value": "Resolved"},
		{"op": "add", "path": "/fields/System.Tags", "value": "stunt"},
	}
	body, status = adoPatchJSON(t, base+"/myorg/MyFirstProject/_apis/wit/workitems/"+strconv.Itoa(int(newID)), patBasic, patchBody)
	if status != 200 {
		t.Fatalf("patch work item -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal patched wi: %v (body %s)", err, body)
	}
	if resp["rev"] != float64(2) {
		t.Fatalf("patched wi rev = %v, want 2", resp["rev"])
	}
	patchedFields := resp["fields"].(map[string]any)
	if patchedFields["System.State"] != "Resolved" {
		t.Fatalf("patched System.State = %v, want Resolved", patchedFields["System.State"])
	}

	// ===== PATCH failure path: unsupported op -> 400 =====

	body, status = adoPatchJSON(t, base+"/myorg/MyFirstProject/_apis/wit/workitems/1", patBasic,
		[]map[string]any{{"op": "bogus", "path": "/fields/System.Title", "value": "x"}})
	if status != 400 {
		t.Fatalf("patch with bogus op -> status %d, want 400; body %s", status, body)
	}

	// ===== List work items by ids =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/wit/workitems?ids=1,"+strconv.Itoa(int(newID)), patBasic)
	if status != 200 {
		t.Fatalf("list workitems by ids -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal workitems list: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) != 2 {
		t.Fatalf("workitems by ids count = %d, want 2 (body %s)", len(value), body)
	}
	if resp["count"] != float64(2) {
		t.Fatalf("workitems count = %v, want 2", resp["count"])
	}

	// ===== WIQL subset: WHERE on System.State =====

	wiqlBody := map[string]any{"query": "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active'"}
	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/wit/wiql", patBasic, wiqlBody)
	if status != 200 {
		t.Fatalf("wiql -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal wiql: %v (body %s)", err, body)
	}
	workItems, ok := resp["workItems"].([]any)
	if !ok || len(workItems) != 1 {
		t.Fatalf("wiql workItems = %v, want exactly the seeded Active bug", resp["workItems"])
	}
	if workItems[0].(map[string]any)["id"] != float64(1) {
		t.Fatalf("wiql matched id = %v, want 1", workItems[0].(map[string]any)["id"])
	}

	// WIQL numeric equality on the System.Id pseudo-field.
	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/wit/wiql", patBasic,
		map[string]any{"query": "SELECT [System.Id] FROM WorkItems WHERE [System.Id] = 1"})
	if status != 200 {
		t.Fatalf("wiql System.Id -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal wiql System.Id: %v (body %s)", err, body)
	}
	workItems, _ = resp["workItems"].([]any)
	if len(workItems) != 1 || workItems[0].(map[string]any)["id"] != float64(1) {
		t.Fatalf("wiql System.Id matches = %v, want exactly id 1", resp["workItems"])
	}

	// ===== WIQL failure path: non-WorkItems FROM -> 400 =====

	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/wit/wiql", patBasic,
		map[string]any{"query": "SELECT [System.Id] FROM Builds"})
	if status != 400 {
		t.Fatalf("wiql bad FROM -> status %d, want 400; body %s", status, body)
	}

	// ===== Git push stores commits; items serve the pushed content =====

	pushBody := map[string]any{
		"refUpdates": []map[string]any{{"name": "refs/heads/main", "oldObjectId": "0000000000000000000000000000000000000000"}},
		"commits": []map[string]any{{
			"comment": "Add docs",
			"changes": []map[string]any{{
				"changeType": "add",
				"item":       map[string]any{"path": "/docs.md"},
				"newContent": map[string]any{"content": "hello docs v1", "contentType": "rawtext"},
			}},
		}},
	}
	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/git/repositories/11111111-0000-0000-0000-000000000001/pushes", patBasic, pushBody)
	if status != 200 {
		t.Fatalf("push -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal push: %v (body %s)", err, body)
	}
	pushCommits := resp["commits"].([]any)
	if len(pushCommits) != 1 {
		t.Fatalf("push commits = %d, want 1", len(pushCommits))
	}
	firstCommit := pushCommits[0].(map[string]any)["commitId"].(string)
	if len(firstCommit) != 40 {
		t.Fatalf("commitId = %q, want 40-char object id", firstCommit)
	}
	refUpdates := resp["refUpdates"].([]any)
	if refUpdates[0].(map[string]any)["newObjectId"].(string) != firstCommit {
		t.Fatalf("newObjectId = %v, want %v", refUpdates[0].(map[string]any)["newObjectId"], firstCommit)
	}

	// Items now serve the pushed content at the pushed path.
	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/git/repositories/11111111-0000-0000-0000-000000000001/items?path=/docs.md", patBasic)
	if status != 200 {
		t.Fatalf("items docs.md -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal item: %v (body %s)", err, body)
	}
	if resp["content"] != "hello docs v1" {
		t.Fatalf("item content = %v, want 'hello docs v1'", resp["content"])
	}
	if resp["commitId"] != firstCommit {
		t.Fatalf("item commitId = %v, want %v", resp["commitId"], firstCommit)
	}

	// Items 404 for unknown paths.
	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/git/repositories/11111111-0000-0000-0000-000000000001/items?path=/nope.md", patBasic)
	if status != 404 {
		t.Fatalf("items unknown path -> status %d, want 404; body %s", status, body)
	}

	// A second push edits the file; versionDescriptor pins the first version.
	pushBody2 := map[string]any{
		"refUpdates": []map[string]any{{"name": "refs/heads/main", "oldObjectId": firstCommit}},
		"commits": []map[string]any{{
			"comment": "Edit docs",
			"changes": []map[string]any{{
				"changeType": "edit",
				"item":       map[string]any{"path": "/docs.md"},
				"newContent": map[string]any{"content": "hello docs v2", "contentType": "rawtext"},
			}},
		}},
	}
	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/git/repositories/11111111-0000-0000-0000-000000000001/pushes", patBasic, pushBody2)
	if status != 200 {
		t.Fatalf("push 2 -> status %d, want 200; body %s", status, body)
	}
	vd := url.QueryEscape(`{"versionType":"commit","version":"` + firstCommit + `"}`)
	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/git/repositories/11111111-0000-0000-0000-000000000001/items?path=/docs.md&versionDescriptor="+vd, patBasic)
	if status != 200 {
		t.Fatalf("items at pinned commit -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal pinned item: %v (body %s)", err, body)
	}
	if resp["content"] != "hello docs v1" {
		t.Fatalf("pinned item content = %v, want 'hello docs v1'", resp["content"])
	}

	// Commits list reflects both pushes.
	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/git/repositories/11111111-0000-0000-0000-000000000001/commits", patBasic)
	if status != 200 {
		t.Fatalf("commits -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal commits: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 3 {
		t.Fatalf("commits count = %d, want >= 3 (seed + 2 pushes)", len(value))
	}

	// ===== Pipelines: list + detail + 404 =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/pipelines", patBasic)
	if status != 200 {
		t.Fatalf("pipelines -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal pipelines: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 1 {
		t.Fatalf("pipelines count = %d, want >= 1", len(value))
	}
	if value[0].(map[string]any)["id"] != float64(1) {
		t.Fatalf("first pipeline id = %v, want 1", value[0].(map[string]any)["id"])
	}

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/pipelines/1", patBasic)
	if status != 200 {
		t.Fatalf("pipeline 1 -> status %d, want 200; body %s", status, body)
	}
	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/pipelines/999", patBasic)
	if status != 404 {
		t.Fatalf("pipeline 999 -> status %d, want 404; body %s", status, body)
	}

	// ===== Queue runs; derive-on-read lifecycle queued -> inProgress -> completed =====

	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/pipelines/1/runs", patBasic, map[string]any{})
	if status != 200 {
		t.Fatalf("queue run -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal queued run: %v (body %s)", err, body)
	}
	runID, ok := resp["id"].(float64)
	if !ok {
		t.Fatalf("queued run id = %v, want number", resp["id"])
	}
	if resp["state"] != "queued" {
		t.Fatalf("freshly queued run state = %v, want queued", resp["state"])
	}
	if resp["pipeline"].(map[string]any)["id"] != float64(1) {
		t.Fatalf("run pipeline id = %v, want 1", resp["pipeline"].(map[string]any)["id"])
	}

	// Failure-injected run (templateParameters.simulate_fail).
	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/pipelines/1/runs", patBasic,
		map[string]any{"templateParameters": map[string]any{"simulate_fail": "true"}})
	if status != 200 {
		t.Fatalf("queue failing run -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal failing run: %v (body %s)", err, body)
	}
	failRunID := resp["id"].(float64)

	// Queue a run on an unknown pipeline -> 404.
	body, status = adoPostJSON(t, base+"/myorg/MyFirstProject/_apis/pipelines/999/runs", patBasic, map[string]any{})
	if status != 404 {
		t.Fatalf("queue run on unknown pipeline -> status %d, want 404; body %s", status, body)
	}

	// Advance the clock past the simulated pipeline duration.
	time.Sleep(3500 * time.Millisecond)

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/pipelines/1/runs/"+strconv.Itoa(int(runID)), patBasic)
	if status != 200 {
		t.Fatalf("get run -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal run: %v (body %s)", err, body)
	}
	if resp["state"] != "completed" {
		t.Fatalf("run state after advance = %v, want completed", resp["state"])
	}
	if resp["result"] != "succeeded" {
		t.Fatalf("run result = %v, want succeeded", resp["result"])
	}
	if resp["finishedDate"] == nil {
		t.Fatalf("finishedDate = nil, want a timestamp once completed")
	}

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/pipelines/1/runs/"+strconv.Itoa(int(failRunID)), patBasic)
	if status != 200 {
		t.Fatalf("get failing run -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal failing run: %v (body %s)", err, body)
	}
	if resp["state"] != "completed" || resp["result"] != "failed" {
		t.Fatalf("failing run state/result = %v/%v, want completed/failed", resp["state"], resp["result"])
	}

	// Runs list agrees with single-run polls.
	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/pipelines/1/runs", patBasic)
	if status != 200 {
		t.Fatalf("list runs -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal runs list: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) != 2 {
		t.Fatalf("runs count = %d, want 2", len(value))
	}
	for _, v := range value {
		r := v.(map[string]any)
		if r["state"] != "completed" {
			t.Fatalf("listed run state = %v, want completed (lists agree with polls)", r["state"])
		}
	}

	// ===== Iterations =====

	body, status = adoGet(t, base+"/myorg/MyFirstProject/_apis/work/teamsettings/iterations", patBasic)
	if status != 200 {
		t.Fatalf("iterations -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal iterations: %v (body %s)", err, body)
	}
	value = resp["value"].([]any)
	if len(value) < 1 {
		t.Fatalf("iterations count = %d, want >= 1", len(value))
	}

	// ===== Bearer auth also works =====

	body, status = adoGet(t, base+"/myorg/_apis/projects", "Bearer testPAT")
	if status != 200 {
		t.Fatalf("projects with bearer -> status %d, want 200; body %s", status, body)
	}
}

// === Azure DevOps test helpers ===

func adoGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func adoPostJSON(t *testing.T, rawurl, auth string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json-patch+json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func adoPatchJSON(t *testing.T, rawurl, auth string, body any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PATCH", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json-patch+json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
