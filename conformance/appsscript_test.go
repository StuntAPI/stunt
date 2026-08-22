package conformance

import (
	"context"
	"encoding/json"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/script/v1"
)

// TestAppsScriptConformance drives Google's generated Apps Script client
// (google.golang.org/api/script/v1) against the apps-script-style adapter:
// project lifecycle, content round-trips, deployments, and scripts.run
// operations through the stock SDK.
func TestAppsScriptConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "apps-script-style")

	svc, err := script.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "conformance-token"})),
	)
	if err != nil {
		t.Fatalf("script.NewService: %v", err)
	}

	// ===== Create (body-less, like the real API) + Get round-trip =====
	created, err := svc.Projects.Create(&script.CreateProjectRequest{}).Do()
	if err != nil {
		t.Fatalf("projects.create: %v", err)
	}
	if created.ScriptId == "" {
		t.Fatalf("projects.create: no scriptId")
	}
	got, err := svc.Projects.Get(created.ScriptId).Do()
	if err != nil {
		t.Fatalf("projects.get: %v", err)
	}
	if got.ScriptId != created.ScriptId {
		t.Errorf("projects.get: scriptId=%q want %q", got.ScriptId, created.ScriptId)
	}
	Record(t, "google-api-go-client", "apps-script-style", "Projects.Create + Projects.Get round-trip")

	// ===== Content update + get round-trips the files =====
	files := []*script.File{
		{Name: "appsscript.json", Type: "JSON", Source: `{"timeZone":"America/Los_Angeles"}`},
		{Name: "Code.gs", Type: "SERVER_JS", Source: "function addNumbers(a, b) { return a + b; }"},
	}
	if _, err := svc.Projects.UpdateContent(created.ScriptId, &script.Content{Files: files}).Do(); err != nil {
		t.Fatalf("projects.content.update: %v", err)
	}
	content, err := svc.Projects.GetContent(created.ScriptId).Do()
	if err != nil {
		t.Fatalf("projects.content.get: %v", err)
	}
	if len(content.Files) != 2 || content.Files[1].Name != "Code.gs" {
		t.Errorf("projects.content.get: files=%+v", content.Files)
	}
	Record(t, "google-api-go-client", "apps-script-style", "Projects content update + get round-trip")

	// ===== scripts.run returns a done Operation with the result =====
	op := mustRun(t, svc, created.ScriptId, &script.ExecutionRequest{
		Function:   "helloWorld",
		Parameters: []any{"SDK"},
	})
	if !op.Done {
		t.Fatalf("scripts.run: done=false")
	}
	var resp struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(op.Response, &resp); err != nil {
		t.Fatalf("scripts.run response decode: %v (%s)", err, op.Response)
	}
	if resp.Result != "Hello, SDK!" {
		t.Errorf("scripts.run: result=%v", resp.Result)
	}
	Record(t, "google-api-go-client", "apps-script-style", "Scripts.Run returns a done Operation with the function result")

	// ===== scripts.run parameters drive the simulated function =====
	addOp := mustRun(t, svc, created.ScriptId, &script.ExecutionRequest{
		Function:   "addNumbers",
		Parameters: []any{float64(19), float64(23)},
	})
	var addResp struct {
		Result float64 `json:"result"`
	}
	if err := json.Unmarshal(addOp.Response, &addResp); err != nil {
		t.Fatalf("scripts.run add decode: %v", err)
	}
	if addResp.Result != 42 {
		t.Errorf("scripts.run add: result=%v", addResp.Result)
	}
	Record(t, "google-api-go-client", "apps-script-style", "Scripts.Run passes parameters into the simulation")

	// ===== Deployments: create + list round-trip =====
	dep, err := svc.Projects.Deployments.Create(created.ScriptId, &script.DeploymentConfig{
		ScriptId:      created.ScriptId,
		VersionNumber: 1,
	}).Do()
	if err != nil {
		t.Fatalf("deployments.create: %v", err)
	}
	deps, err := svc.Projects.Deployments.List(created.ScriptId).Do()
	if err != nil {
		t.Fatalf("deployments.list: %v", err)
	}
	found := false
	for _, d := range deps.Deployments {
		if d.DeploymentId == dep.DeploymentId {
			found = true
		}
	}
	if !found {
		t.Errorf("deployments.list: created %q missing (%+v)", dep.DeploymentId, deps.Deployments)
	}
	Record(t, "google-api-go-client", "apps-script-style", "Deployments.Create + Deployments.List round-trip")

	// ===== Unknown project content read decodes googleapi 404 =====
	_, err = svc.Projects.GetContent("sdk-missing-script").Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("projects.content.get unknown: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "apps-script-style", "Unknown project content Get -> googleapi 404")
}

func mustRun(t *testing.T, svc *script.Service, scriptID string, req *script.ExecutionRequest) *script.Operation {
	t.Helper()
	op, err := svc.Scripts.Run(scriptID, req).Do()
	if err != nil {
		t.Fatalf("scripts.run: %v", err)
	}
	return op
}
