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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestAzureServiceBusStyleAdapter exercises the azure-servicebus-style adapter:
//
//   - SAS token auth required: 401 without auth
//   - Service Bus send message → 201
//   - Service Bus receive message → 200 with body
//   - Receive again → 204 (empty)
//   - Topic info
//   - Storage queue send (JSON) → 201 XML response
//   - Storage queue receive → XML with MessageText
func TestAzureServiceBusStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "azure-servicebus-style")
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
			"sb": {Adapter: absAdapterDir},
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

	base := addrs["sb"]
	sasToken := "SharedAccessSignature sr=https%3A%2F%2Fmybus.servicebus.windows.net%2Fmyqueue&sig=fakesig&se=1900000000&skn=RootManageSharedAccessKey"

	// ===== 401 without auth =====

	_, status := sbPostJSON(t, base+"/myqueue/messages", "", map[string]any{"Body": "hello"})
	if status != 401 {
		t.Fatalf("send without auth -> status %d, want 401", status)
	}

	// ===== Service Bus send → 201 =====

	body, status := sbPostJSON(t, base+"/myqueue/messages", sasToken, map[string]any{
		"Body":        "Hello Service Bus!",
		"ContentType": "text/plain",
	})
	if status != 201 {
		t.Fatalf("send -> status %d, want 201; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if _, ok := resp["MessageId"].(string); !ok {
		t.Fatalf("MessageId = %v, want string", resp["MessageId"])
	}
	if _, ok := resp["LockToken"].(string); !ok {
		t.Fatalf("LockToken = %v, want string", resp["LockToken"])
	}

	// ===== Service Bus receive → 200 with the sent body =====

	body, status = sbDelete(t, base+"/myqueue/messages/head", sasToken)
	if status != 200 {
		t.Fatalf("receive -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal receive: %v (body %s)", err, body)
	}
	if resp["Body"] != "Hello Service Bus!" {
		t.Fatalf("received Body = %v, want 'Hello Service Bus!'", resp["Body"])
	}
	if resp["ContentType"] != "text/plain" {
		t.Fatalf("received ContentType = %v, want 'text/plain'", resp["ContentType"])
	}

	// ===== Receive again → 204 (empty) =====

	body, status = sbDelete(t, base+"/myqueue/messages/head", sasToken)
	if status != 204 {
		t.Fatalf("receive (empty) -> status %d, want 204; body %s", status, body)
	}

	// ===== Topic info =====

	body, status = sbGet(t, base+"/$topicInfo?api-version=2024-01-01", sasToken)
	if status != 200 {
		t.Fatalf("topic info -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal topic: %v (body %s)", err, body)
	}
	if _, ok := resp["properties"].(map[string]any); !ok {
		t.Fatalf("properties = %v, want object", resp["properties"])
	}

	// ===== Storage queue send (JSON) → 201 XML response =====

	resp2 := sbPostRaw(t, base+"/mystorage/myqueue/messages", sasToken,
		`{"MessageText":"Storage message"}`, "application/json")
	defer resp2.Body.Close()
	b, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 201 {
		t.Fatalf("storage send -> status %d, want 201; body %s", resp2.StatusCode, string(b))
	}
	if !strings.Contains(string(b), "<MessageId>") {
		t.Fatalf("storage send response should contain XML, got: %s", string(b))
	}

	// ===== Storage queue receive → XML with MessageText =====

	resp3 := sbGetRaw(t, base+"/mystorage/myqueue/messages", sasToken)
	defer resp3.Body.Close()
	b, _ = io.ReadAll(resp3.Body)
	if resp3.StatusCode != 200 {
		t.Fatalf("storage receive -> status %d, want 200; body %s", resp3.StatusCode, string(b))
	}
	if !strings.Contains(string(b), "Storage message") {
		t.Fatalf("storage receive should contain MessageText, got: %s", string(b))
	}
	if !strings.Contains(string(b), "<QueueMessagesList>") {
		t.Fatalf("storage receive should contain <QueueMessagesList>, got: %s", string(b))
	}

	// ===== Bearer also works =====

	_, status = sbPostJSON(t, base+"/myqueue/messages", "Bearer testtoken", map[string]any{"Body": "bearer test"})
	if status != 201 {
		t.Fatalf("send with bearer -> status %d, want 201", status)
	}
}

// === Azure Service Bus test helpers ===

func sbPostJSON(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
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

func sbPostRaw(t *testing.T, rawurl, auth, body, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func sbDelete(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
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

func sbGet(t *testing.T, rawurl, auth string) (string, int) {
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

func sbGetRaw(t *testing.T, rawurl, auth string) *http.Response {
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
	return resp
}
