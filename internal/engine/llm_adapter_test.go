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

// TestLLMStyleAdapter exercises the LLM-style adapter (OpenAI + Anthropic)
// end-to-end through the chat completion, messages, and models flows,
// asserting deterministic responses:
//
//   - POST /v1/chat/completions -> choices[0].message.content non-empty + deterministic
//   - POST /v1/chat/completions (stream) -> SSE with [DONE]
//   - POST /v1/messages -> content[0].text non-empty + deterministic
//   - GET /v1/models -> {object: "list", data: [...]}
func TestLLMStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "llm-style")
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
			"llm": {Adapter: absAdapterDir},
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

	base := addrs["llm"]
	const apiKey = "sk-test-key"

	// ===== 401: no auth on chat completions =====
	_, status := llmPostBearer(t, base+"/v1/chat/completions", "", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	if status != 401 {
		t.Fatalf("POST /v1/chat/completions (no auth) -> status %d, want 401", status)
	}

	// ===== OpenAI chat completions -> deterministic response =====
	body, status := llmPostBearer(t, base+"/v1/chat/completions", apiKey, map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "What is 2+2?"}},
	})
	if status != 200 {
		t.Fatalf("POST /v1/chat/completions -> status %d, want 200; body %s", status, body)
	}
	var chatResp map[string]any
	if err := json.Unmarshal([]byte(body), &chatResp); err != nil {
		t.Fatalf("unmarshal chat response: %v (body %s)", err, body)
	}
	if chatResp["object"] != "chat.completion" {
		t.Fatalf("object = %v, want 'chat.completion'", chatResp["object"])
	}
	id, ok := chatResp["id"].(string)
	if !ok || !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("id = %v, want chatcmpl-* prefix", chatResp["id"])
	}
	if chatResp["model"] != "gpt-4o" {
		t.Fatalf("model = %v, want gpt-4o", chatResp["model"])
	}
	choices, ok := chatResp["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %v, want array of 1", chatResp["choices"])
	}
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Fatalf("message.role = %v, want 'assistant'", msg["role"])
	}
	content, ok := msg["content"].(string)
	if !ok || content == "" {
		t.Fatalf("message.content = %v, want non-empty string", msg["content"])
	}
	// Deterministic policy: content must be "Echo: <last user message>".
	wantContent := "Echo: What is 2+2?"
	if content != wantContent {
		t.Fatalf("content = %q, want %q (deterministic echo policy)", content, wantContent)
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v, want 'stop'", choice["finish_reason"])
	}
	// usage must be present.
	usage, ok := chatResp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %v, want a dict", chatResp["usage"])
	}
	if _, ok := usage["total_tokens"].(float64); !ok {
		t.Fatalf("usage.total_tokens = %v, want a number", usage["total_tokens"])
	}

	// ===== Determinism: same input → same output =====
	body2, _ := llmPostBearer(t, base+"/v1/chat/completions", apiKey, map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "What is 2+2?"}},
	})
	var chatResp2 map[string]any
	json.Unmarshal([]byte(body2), &chatResp2)
	content2 := chatResp2["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]
	if content2 != content {
		t.Fatalf("non-deterministic: first=%q second=%q", content, content2)
	}

	// ===== OpenAI chat completions (stream) -> SSE with [DONE] =====
	streamBody, streamStatus := llmPostBearerRaw(t, base+"/v1/chat/completions", apiKey, map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]any{{"role": "user", "content": "stream me"}},
		"stream":   true,
	})
	if streamStatus != 200 {
		t.Fatalf("POST /v1/chat/completions (stream) -> status %d, want 200", streamStatus)
	}
	if !strings.Contains(streamBody, "data: ") {
		t.Fatalf("stream body missing 'data:' SSE prefix: %s", streamBody)
	}
	if !strings.Contains(streamBody, "[DONE]") {
		t.Fatalf("stream body missing [DONE]: %s", streamBody)
	}

	// ===== GET /v1/models -> {object: "list", data: [...]} =====
	body, status = llmGetBearer(t, base+"/v1/models", apiKey)
	if status != 200 {
		t.Fatalf("GET /v1/models -> status %d, want 200; body %s", status, body)
	}
	var modelsResp map[string]any
	if err := json.Unmarshal([]byte(body), &modelsResp); err != nil {
		t.Fatalf("unmarshal models: %v (body %s)", err, body)
	}
	if modelsResp["object"] != "list" {
		t.Fatalf("object = %v, want 'list'", modelsResp["object"])
	}
	modelData, ok := modelsResp["data"].([]any)
	if !ok || len(modelData) == 0 {
		t.Fatalf("data = %v, want non-empty array", modelsResp["data"])
	}
	firstModel := modelData[0].(map[string]any)
	if firstModel["object"] != "model" {
		t.Fatalf("model object = %v, want 'model'", firstModel["object"])
	}
	if _, ok := firstModel["id"].(string); !ok {
		t.Fatalf("model id = %v, want string", firstModel["id"])
	}

	// ===== 401: no x-api-key on Anthropic messages =====
	_, status = llmPostHeaders(t, base+"/v1/messages", map[string]any{}, map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	if status != 401 {
		t.Fatalf("POST /v1/messages (no x-api-key) -> status %d, want 401", status)
	}

	// ===== Anthropic messages -> deterministic response =====
	body, status = llmPostHeaders(t, base+"/v1/messages", map[string]any{
		"x-api-key":         apiKey,
		"anthropic-version": "2023-06-01",
	}, map[string]any{
		"model":      "claude-3-5-sonnet-20241022",
		"max_tokens": 100,
		"messages":   []map[string]any{{"role": "user", "content": "Tell me a joke."}},
	})
	if status != 200 {
		t.Fatalf("POST /v1/messages -> status %d, want 200; body %s", status, body)
	}
	var msgResp map[string]any
	if err := json.Unmarshal([]byte(body), &msgResp); err != nil {
		t.Fatalf("unmarshal messages response: %v (body %s)", err, body)
	}
	if msgResp["type"] != "message" {
		t.Fatalf("type = %v, want 'message'", msgResp["type"])
	}
	if msgResp["role"] != "assistant" {
		t.Fatalf("role = %v, want 'assistant'", msgResp["role"])
	}
	msgID, ok := msgResp["id"].(string)
	if !ok || !strings.HasPrefix(msgID, "msg_stunt_") {
		t.Fatalf("id = %v, want msg_stunt_* prefix", msgResp["id"])
	}
	contentBlocks, ok := msgResp["content"].([]any)
	if !ok || len(contentBlocks) != 1 {
		t.Fatalf("content = %v, want array of 1", msgResp["content"])
	}
	block := contentBlocks[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("content[0].type = %v, want 'text'", block["type"])
	}
	anthContent, ok := block["text"].(string)
	if !ok || anthContent == "" {
		t.Fatalf("content[0].text = %v, want non-empty string", block["text"])
	}
	// Deterministic policy check.
	wantAnthContent := "Echo: Tell me a joke."
	if anthContent != wantAnthContent {
		t.Fatalf("content[0].text = %q, want %q (deterministic echo policy)", anthContent, wantAnthContent)
	}
	if msgResp["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want 'end_turn'", msgResp["stop_reason"])
	}
	anthUsage, ok := msgResp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage = %v, want a dict", msgResp["usage"])
	}
	if _, ok := anthUsage["input_tokens"].(float64); !ok {
		t.Fatalf("usage.input_tokens = %v, want a number", anthUsage["input_tokens"])
	}
}

// llmPostBearer performs a JSON POST with a Bearer token and returns body + status.
func llmPostBearer(t *testing.T, url, apiKey string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// llmPostBearerRaw is like llmPostBearer but returns the body as a string
// (used for SSE streaming where the body is not JSON).
func llmPostBearerRaw(t *testing.T, url, apiKey string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// llmGetBearer performs an authenticated GET with a Bearer token and returns body + status.
func llmGetBearer(t *testing.T, url, apiKey string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// llmPostHeaders performs a JSON POST with arbitrary headers and returns body + status.
func llmPostHeaders(t *testing.T, url string, headers map[string]any, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v.(string))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
