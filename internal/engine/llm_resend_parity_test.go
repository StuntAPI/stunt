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

	"stuntapi.com/stunt/internal/manifest"
)

func TestLLMResendClientParity(t *testing.T) {
	for _, adapter := range []string{"llm-style", "resend-style"} {
		t.Run(adapter, func(t *testing.T) {
			dir, err := filepath.Abs(filepath.Join("..", "..", "adapters", adapter))
			if err != nil {
				t.Fatal(err)
			}
			e, err := New(&manifest.Manifest{Path: filepath.Join(t.TempDir(), "stunt.yaml"), Version: 1, Network: manifest.Network{Mode: "port"}, Services: map[string]manifest.Service{"api": {Adapter: dir}}})
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			addrs, cancel, err := e.ServeForTest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer cancel()
			request := func(method, path, key string, body any) (int, []byte) {
				t.Helper()
				raw, _ := json.Marshal(body)
				req, _ := http.NewRequest(method, addrs["api"]+path, bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				if adapter == "llm-style" {
					req.Header.Set("x-api-key", key)
				} else {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				data, _ := io.ReadAll(res.Body)
				return res.StatusCode, data
			}
			if adapter == "llm-style" {
				status, body := request("GET", "/v1/models", "sk-test-key", nil)
				if status != 200 || !bytes.Contains(body, []byte(`"type":"model"`)) {
					t.Fatalf("models: %d %s", status, body)
				}
				status, _ = request("GET", "/v1/models", "invalid", nil)
				if status != 401 {
					t.Fatalf("invalid key: %d", status)
				}
				status, body = request("POST", "/v1/messages", "sk-test-key", map[string]any{"model": "synthetic-model", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "synthetic prompt"}}})
				if status != 200 {
					t.Fatalf("stream: %d %s", status, body)
				}
				for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
					if !strings.Contains(string(body), "event: "+event+"\n") {
						t.Errorf("missing %s in %s", event, body)
					}
				}
				if !bytes.Contains(body, []byte("Echo: synthetic prompt")) {
					t.Fatalf("missing echo: %s", body)
				}
			} else {
				status, body := request("GET", "/domains", "synthetic-key", nil)
				if status != 200 || !bytes.Contains(body, []byte(`"data":[]`)) {
					t.Fatalf("domains: %d %s", status, body)
				}
				status, _ = request("GET", "/domains", "", nil)
				if status != 401 {
					t.Fatalf("unauthenticated domains: %d", status)
				}
				status, body = request("POST", "/emails/batch", "synthetic-key", []any{map[string]any{"from": "sender@example.test", "to": "one@example.test", "subject": "first"}, map[string]any{"from": "sender@example.test", "to": "two@example.test", "subject": "second"}})
				var result struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if err := json.Unmarshal(body, &result); err != nil {
					t.Fatal(err)
				}
				if status != 200 || len(result.Data) != 2 || result.Data[0].ID == result.Data[1].ID {
					t.Fatalf("batch: %d %s", status, body)
				}
				for _, entry := range result.Data {
					status, body = request("GET", "/emails/"+entry.ID, "synthetic-key", nil)
					if status != 200 {
						t.Fatalf("retrieve: %d %s", status, body)
					}
				}
				status, _ = request("POST", "/emails/batch", "synthetic-key", []any{})
				if status != 422 {
					t.Fatalf("empty batch: %d", status)
				}
			}
		})
	}
}
