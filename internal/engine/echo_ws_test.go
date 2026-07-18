package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestEchoStyleWebSocket loads the committed echo-style adapter (which
// declares a /ws/echo WebSocket route) and exercises it with a real
// coder/websocket client: connect, send messages, assert each is echoed,
// verify the kv counter advanced, then close and confirm clean shutdown.
func TestEchoStyleWebSocket(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "echo-style")
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
			"echo": {Adapter: absAdapterDir},
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

	base := addrs["echo"]

	// Connect to the /ws/echo route.
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	c, _, err := websocket.Dial(ctx, base+"/ws/echo", &websocket.DialOptions{
		Subprotocols: []string{"echo.v1"},
	})
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	// Subprotocol negotiation.
	if got := c.Subprotocol(); got != "echo.v1" {
		t.Errorf("subprotocol = %q, want %q", got, "echo.v1")
	}

	// Send a few text messages and assert each is echoed.
	messages := []string{"hello", "world", "synthetic-echo"}
	for _, msg := range messages {
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.Write(wctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		wcancel()

		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, data, err := c.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("read after send %q: %v", msg, err)
		}
		if string(data) != msg {
			t.Errorf("echo = %q, want %q", string(data), msg)
		}
	}

	// Send a JSON object and verify it round-trips as JSON (dict echo).
	jsonMsg := map[string]any{"type": "ping", "seq": float64(42)}
	jsonBytes, _ := json.Marshal(jsonMsg)
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := c.Write(wctx, websocket.MessageText, jsonBytes); err != nil {
		t.Fatalf("write JSON: %v", err)
	}
	wcancel()

	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, data, err := c.Read(rctx)
	rcancel()
	if err != nil {
		t.Fatalf("read JSON echo: %v", err)
	}
	var echoed map[string]any
	if err := json.Unmarshal(data, &echoed); err != nil {
		t.Fatalf("echo JSON unmarshal: %v (raw %q)", err, string(data))
	}
	if echoed["type"] != "ping" {
		t.Errorf("echo JSON type = %v, want ping", echoed["type"])
	}
	if echoed["seq"].(float64) != 42 {
		t.Errorf("echo JSON seq = %v, want 42", echoed["seq"])
	}

	// Verify the kv counter advanced (4 messages echoed → counter >= 4).
	// The handler increments ws_echo_count on every message; the kv store
	// is shared across all Starlark handlers for this service.
	st := e.states["echo"]
	if st == nil {
		t.Fatal("missing echo service state")
	}
	val, err := st.kvStore.Get("echo", "ws_echo_count")
	if err != nil {
		t.Fatalf("kv get ws_echo_count: %v", err)
	}
	kvCount := 0
	if val != "" {
		if _, err := fmt.Sscanf(val, "%d", &kvCount); err != nil {
			t.Fatalf("kv ws_echo_count = %q, not an int", val)
		}
	}
	if kvCount < 4 {
		t.Errorf("ws_echo_count = %d, want >= 4", kvCount)
	}

	// Close the connection and confirm clean shutdown (no hang).
	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client close: %v", err)
	}

	// Give the server handler a moment to notice the disconnect.
	time.Sleep(100 * time.Millisecond)

	// Verify the server is still healthy by making a fresh connection.
	ctx2, cancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel3()
	c2, _, err := websocket.Dial(ctx2, base+"/ws/echo", nil)
	if err != nil {
		t.Fatalf("reconnect after close: %v", err)
	}
	defer c2.Close(websocket.StatusNormalClosure, "")

	wctx2, wcancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	if err := c2.Write(wctx2, websocket.MessageText, []byte("after-reconnect")); err != nil {
		t.Fatalf("write after reconnect: %v", err)
	}
	wcancel2()

	rctx2, rcancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	_, data2, err := c2.Read(rctx2)
	rcancel2()
	if err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	if string(data2) != "after-reconnect" {
		t.Errorf("post-reconnect echo = %q, want %q", string(data2), "after-reconnect")
	}
}
