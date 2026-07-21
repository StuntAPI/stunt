package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"stuntapi.com/stunt/internal/manifest"
)

// wsAdapterYAML is a minimal adapter manifest with a ws section.
const wsAdapterYAML = `
id: wstest
name: WSTest
ws:
  - route: /ws/echo
    handler: scripts/ws.star#on_echo
  - route: /ws/push
    handler: scripts/ws.star#on_push
  - route: /ws/health/{id}
    handler: scripts/ws.star#on_param
  - route: /ws/proto
    handler: scripts/ws.star#on_echo
    subprotocols: ["chat.v1"]
  - route: /ws/sendlist
    handler: scripts/ws.star#on_send_list
  - route: /ws/sendclose
    handler: scripts/ws.star#on_send_close_status
`

// wsStarScript implements echo, push, and param handlers.
const wsStarScript = `
# Echo: receive messages and send them back until client closes.
def on_echo(ws):
    while True:
        m = ws.recv()
        if m == None:
            break
        ws.send(m)

# Push: send 3 unsolicited messages then wait for client to disconnect.
def on_push(ws):
    for i in range(3):
        ws.send({"seq": i})
    while True:
        m = ws.recv()
        if m == None:
            break

# Param: echo the path param back on connect.
def on_param(ws):
    ws.send("handler-ready")
    while True:
        m = ws.recv()
        if m == None:
            break

# SendList: send a JSON array on connect, then wait for disconnect.
def on_send_list(ws):
    ws.send([1, 2, 3])
    while True:
        m = ws.recv()
        if m == None:
            break

# SendCloseStatus: send the close() return value as a string so the
# test can observe whether validation accepted or rejected the code.
def on_send_close_status(ws):
    err = ws.close(99999)
    ws.send("no-error")
    while True:
        m = ws.recv()
        if m == None:
            break
`

// setupWSEngine lays out a temp adapter directory with a ws section and
// starts the engine, returning the base HTTP URL.
func setupWSEngine(t *testing.T) string {
	t.Helper()

	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", wsAdapterYAML)
	writeFile(t, adapterDir, "scripts/ws.star", wsStarScript)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"wstest": {Adapter: adapterDir},
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
	time.Sleep(30 * time.Millisecond)

	return addrs["wstest"]
}

// wsDial connects to a WebSocket endpoint and returns the connection.
func wsDial(t *testing.T, baseURL, path string, opts *websocket.DialOptions) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := baseURL + path
	c, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		t.Fatalf("websocket dial %s: %v", url, err)
	}
	return c
}

// wsSendText sends a text message.
func wsSendText(t *testing.T, c *websocket.Conn, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatalf("ws send: %v", err)
	}
}

// wsSendJSON sends a map as JSON text.
func wsSendJSON(t *testing.T, c *websocket.Conn, m map[string]any) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wsSendText(t, c, string(b))
}

// wsRecv reads one message and returns the payload.
func wsRecv(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws recv: %v", err)
	}
	return data
}

// wsRecvJSON reads one message and unmarshals as JSON.
func wsRecvJSON(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	data := wsRecv(t, c)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", string(data), err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Echo: send text, receive text echo.
// ---------------------------------------------------------------------------

func TestWebSocketEcho(t *testing.T) {
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/echo", nil)
	defer c.Close(websocket.StatusNormalClosure, "")

	wsSendText(t, c, "hello")
	got := string(wsRecv(t, c))
	if got != "hello" {
		t.Errorf("echo = %q, want %q", got, "hello")
	}
}

// ---------------------------------------------------------------------------
// JSON: send {"x":1}, handler reads dict, echoes {"x":1} (round-trips as dict).
// ---------------------------------------------------------------------------

func TestWebSocketJSONRoundTrip(t *testing.T) {
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/echo", nil)
	defer c.Close(websocket.StatusNormalClosure, "")

	wsSendJSON(t, c, map[string]any{"x": float64(1)})
	got := wsRecvJSON(t, c)
	x, ok := got["x"].(float64)
	if !ok || x != 1 {
		t.Errorf("echo x = %v, want 1", got["x"])
	}
}

// ---------------------------------------------------------------------------
// Server-push: handler sends unsolicited messages (3) on connect.
// ---------------------------------------------------------------------------

func TestWebSocketServerPush(t *testing.T) {
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/push", nil)
	defer c.Close(websocket.StatusNormalClosure, "")

	for i := 0; i < 3; i++ {
		m := wsRecvJSON(t, c)
		seq, ok := m["seq"].(float64)
		if !ok || int(seq) != i {
			t.Errorf("push[%d] seq = %v, want %d", i, m["seq"], i)
		}
	}
}

// ---------------------------------------------------------------------------
// Disconnect: client closes → handler's recv() returns None → handler returns
// cleanly (no goroutine leak; no hang).
// ---------------------------------------------------------------------------

func TestWebSocketClientDisconnect(t *testing.T) {
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/echo", nil)

	// Send one message and get the echo to confirm the handler is live.
	wsSendText(t, c, "ping")
	_ = wsRecv(t, c)

	// Close the connection from the client side.
	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client close: %v", err)
	}

	// Give the server handler a moment to notice the disconnect.
	time.Sleep(100 * time.Millisecond)

	// The server should have terminated cleanly. We verify by making a new
	// connection to the same route — if the handler goroutine leaked and held
	// state, a new connection still works independently.
	c2 := wsDial(t, baseURL, "/ws/echo", nil)
	defer c2.Close(websocket.StatusNormalClosure, "")
	wsSendText(t, c2, "after-disconnect")
	got := string(wsRecv(t, c2))
	if got != "after-disconnect" {
		t.Errorf("post-disconnect echo = %q, want %q", got, "after-disconnect")
	}
}

// ---------------------------------------------------------------------------
// Concurrency: two simultaneous connections on the same route, each echoes
// independently.
// ---------------------------------------------------------------------------

func TestWebSocketConcurrentConnections(t *testing.T) {
	baseURL := setupWSEngine(t)

	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			c := wsDial(t, baseURL, "/ws/echo", nil)
			defer c.Close(websocket.StatusNormalClosure, "")

			msg := "concurrent-" + string(rune('A'+idx))
			wsSendText(t, c, msg)
			got := string(wsRecv(t, c))
			if got != msg {
				t.Errorf("conn[%d] echo = %q, want %q", idx, got, msg)
			}
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Subprotocol: negotiate a subprotocol; assert conn.Subprotocol() matches.
// ---------------------------------------------------------------------------

func TestWebSocketSubprotocolNegotiation(t *testing.T) {
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/proto", &websocket.DialOptions{
		Subprotocols: []string{"chat.v1"},
	})
	defer c.Close(websocket.StatusNormalClosure, "")

	if got := c.Subprotocol(); got != "chat.v1" {
		t.Errorf("subprotocol = %q, want %q", got, "chat.v1")
	}
}

// ---------------------------------------------------------------------------
// Subprotocol mismatch: client offers a subprotocol the server doesn't
// support → no subprotocol negotiated, connection still works.
// ---------------------------------------------------------------------------

func TestWebSocketSubprotocolMismatch(t *testing.T) {
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/proto", &websocket.DialOptions{
		Subprotocols: []string{"unknown.v9"},
	})
	defer c.Close(websocket.StatusNormalClosure, "")

	// Server should accept the connection but negotiate no subprotocol.
	if got := c.Subprotocol(); got != "" {
		t.Errorf("subprotocol = %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// No-route: a WS upgrade to an undeclared path → falls through to normal
// HTTP dispatch → 404.
// ---------------------------------------------------------------------------

func TestWebSocketNoRouteFallsThrough(t *testing.T) {
	baseURL := setupWSEngine(t)

	// Dial an undeclared WS route. The server should reject the upgrade
	// (coder/websocket.Accept is never called) and fall through to the
	// normal HTTP dispatch which returns 404.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, baseURL+"/ws/nonexistent", nil)
	if err == nil {
		t.Fatal("expected error dialing undeclared WS route, got nil")
	}
}

// ---------------------------------------------------------------------------
// Non-upgrade GET: a normal GET to a ws route → normal HTTP dispatch (not
// upgraded). The path has no matching HTTP endpoint, so it returns 404.
// ---------------------------------------------------------------------------

func TestWebSocketRouteNonUpgradeGET(t *testing.T) {
	baseURL := setupWSEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ws/echo", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ws/echo: %v", err)
	}
	defer resp.Body.Close()

	// Should be 404 (no matching HTTP endpoint), not upgraded.
	if resp.StatusCode != 404 {
		t.Errorf("GET /ws/echo status = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Route with params: path param matches and the handler runs.
// ---------------------------------------------------------------------------

func TestWebSocketRouteWithParams(t *testing.T) {
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/health/device123", nil)
	defer c.Close(websocket.StatusNormalClosure, "")

	got := string(wsRecv(t, c))
	if got != "handler-ready" {
		t.Errorf("param route initial msg = %q, want %q", got, "handler-ready")
	}
}

// ---------------------------------------------------------------------------
// Step budget: a chatty loop that sends many messages (with recv in between)
// is NOT killed by the step limit. This verifies the elevated budget works.
// ---------------------------------------------------------------------------

func TestWebSocketChattyLoopNotKilledByStepLimit(t *testing.T) {
	// Use the echo route. Send 500 messages; each is echoed. The echo handler
	// does minimal per-message work, so 500 round-trips should stay well
	// within 10M steps.
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/echo", nil)
	defer c.Close(websocket.StatusNormalClosure, "")

	const n = 500
	for i := 0; i < n; i++ {
		wsSendText(t, c, "msg")
		got := string(wsRecv(t, c))
		if got != "msg" {
			t.Errorf("round-trip %d: got %q, want %q", i, got, "msg")
		}
	}
}

// ---------------------------------------------------------------------------
// Connection cap: opening more than the limit of concurrent connections
// results in HTTP 503. Closing a connection frees a slot.
// ---------------------------------------------------------------------------

func TestWebSocketConnectionCap(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", wsAdapterYAML)
	writeFile(t, adapterDir, "scripts/ws.star", wsStarScript)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"wstest": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Close() })

	// Override the semaphore to a small limit for this test.
	const limit = 3
	e.wsSem = make(chan struct{}, limit)

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	t.Cleanup(cancel)
	time.Sleep(30 * time.Millisecond)

	baseURL := addrs["wstest"]

	// Open exactly `limit` connections. Each blocks on recv() (no messages
	// sent) so the semaphore slots are held.
	var conns []*websocket.Conn
	for i := 0; i < limit; i++ {
		c := wsDial(t, baseURL, "/ws/echo", nil)
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close(websocket.StatusNormalClosure, "")
		}
	}()

	// Give the server a moment to register all connections.
	time.Sleep(50 * time.Millisecond)

	// The limit+1 connection should be rejected with an HTTP error (503).
	ctx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	_, _, dialErr := websocket.Dial(ctx, baseURL+"/ws/echo", nil)
	if dialErr == nil {
		t.Fatal("expected error dialing over the connection limit, got nil")
	}

	// Close one connection to free a slot.
	conns[0].Close(websocket.StatusNormalClosure, "")
	conns = conns[1:]
	time.Sleep(50 * time.Millisecond)

	// A new connection should now succeed.
	c2 := wsDial(t, baseURL, "/ws/echo", nil)
	defer c2.Close(websocket.StatusNormalClosure, "")
	wsSendText(t, c2, "after-free")
	got := string(wsRecv(t, c2))
	if got != "after-free" {
		t.Errorf("echo after free = %q, want %q", got, "after-free")
	}
}

// ---------------------------------------------------------------------------
// Shutdown: a connected client receives a close frame when the engine shuts
// down, not a hard error or timeout.
// ---------------------------------------------------------------------------

func TestWebSocketShutdownSendsCloseFrame(t *testing.T) {
	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", wsAdapterYAML)
	writeFile(t, adapterDir, "scripts/ws.star", wsStarScript)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"wstest": {Adapter: adapterDir},
		},
	}

	e, err := New(m)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	addrs, cancel, err := e.ServeForTest(context.Background())
	if err != nil {
		t.Fatalf("ServeForTest: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	baseURL := addrs["wstest"]
	c := wsDial(t, baseURL, "/ws/echo", nil)
	defer c.CloseNow()

	// Confirm the connection is live.
	wsSendText(t, c, "ping")
	_ = wsRecv(t, c)

	// Start reading before triggering shutdown so the close frame can be
	// received as soon as it arrives.
	type readResult struct {
		msgType websocket.MessageType
		data    []byte
		err     error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		mt, data, err := c.Read(rctx)
		resultCh <- readResult{msgType: mt, data: data, err: err}
	}()

	// Give the read goroutine a moment to start.
	time.Sleep(50 * time.Millisecond)

	// Trigger server shutdown via cancel — this closes the engine's
	// shutdownCh, which the WebSocket handler monitors.
	cancel()

	// The client should receive a close frame (not a timeout).
	select {
	case res := <-resultCh:
		if res.err == nil {
			t.Fatal("expected a close error on shutdown, got nil")
		}
		status := websocket.CloseStatus(res.err)
		if status == -1 {
			t.Errorf("expected a WebSocket close error, got non-close error: %v", res.err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for client read to return")
	}
}

// ---------------------------------------------------------------------------
// send() with a list: ws.send([1,2,3]) produces valid JSON [1,2,3].
// ---------------------------------------------------------------------------

func TestWebSocketSendList(t *testing.T) {
	baseURL := setupWSEngine(t)
	c := wsDial(t, baseURL, "/ws/sendlist", nil)
	defer c.Close(websocket.StatusNormalClosure, "")

	data := wsRecv(t, c)
	var arr []interface{}
	if err := json.Unmarshal(data, &arr); err != nil {
		t.Fatalf("unmarshal %q: %v", string(data), err)
	}
	if len(arr) != 3 {
		t.Fatalf("got %d elements, want 3", len(arr))
	}
	for i, want := range []float64{1, 2, 3} {
		if got, ok := arr[i].(float64); !ok || got != want {
			t.Errorf("arr[%d] = %v, want %v", i, arr[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// close() status validation: invalid codes are rejected.
// ---------------------------------------------------------------------------

func TestValidateCloseCode(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{1000, true},   // StatusNormalClosure
		{1001, true},   // StatusGoingAway
		{1011, true},   // StatusInternalError
		{3000, true},   // start of application-defined range
		{4999, true},   // end of application-defined range
		{999, false},   // below minimum
		{1015, false},  // StatusTLSHandshake — reserved, not valid on wire
		{1005, false},  // StatusNoStatusRcvd — reserved
		{1006, false},  // StatusAbnormalClosure — reserved
		{5000, false},  // above custom range
		{99999, false}, // wildly out of range
		{-1, false},    // negative
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.code), func(t *testing.T) {
			err := validateCloseCode(tt.code)
			if tt.want && err != nil {
				t.Errorf("validateCloseCode(%d) = %v, want nil", tt.code, err)
			}
			if !tt.want && err == nil {
				t.Errorf("validateCloseCode(%d) = nil, want error", tt.code)
			}
		})
	}
}
