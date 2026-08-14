package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"stuntapi.com/stunt/internal/manifest"
)

func bootDiscord(t *testing.T) (base string, cleanup func()) {
	t.Helper()
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "discord-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"discord": {Adapter: adapterDir},
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
	time.Sleep(50 * time.Millisecond)
	return addrs["discord"], func() {
		cancel()
		e.Close()
	}
}

// TestDiscordStyleGateway drives the WebSocket Gateway handshake: server HELLO
// → client IDENTIFY → server READY → dispatched MESSAGE_CREATE.
func TestDiscordStyleGateway(t *testing.T) {
	base, cleanup := bootDiscord(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, base+"/gateway", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	read := func() map[string]any {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, data, err := c.Read(rctx)
		rcancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal: %v (raw %s)", err, string(data))
		}
		return m
	}
	write := func(m map[string]any) {
		b, _ := json.Marshal(m)
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.Write(wctx, websocket.MessageText, b); err != nil {
			t.Fatalf("write: %v", err)
		}
		wcancel()
	}

	hello := read()
	if hello["op"].(float64) != 10 {
		t.Fatalf("HELLO op = %v, want 10", hello["op"])
	}

	write(map[string]any{"op": 2, "d": map[string]any{"token": "mock-bot-token"}})

	ready := read()
	if ready["t"] != "READY" {
		t.Fatalf("expected READY, got t=%v", ready["t"])
	}

	mc := read()
	if mc["t"] != "MESSAGE_CREATE" {
		t.Fatalf("expected MESSAGE_CREATE, got t=%v", mc["t"])
	}
	if d, _ := mc["d"].(map[string]any); d == nil || d["content"] == nil {
		t.Fatalf("MESSAGE_CREATE has no content: %v", mc["d"])
	}
}

// The discord adapter's documented mock Ed25519 private key (matches
// adapters/discord-style _ED25519_PRIVATE_KEY). Used to sign test interactions
// the way Discord would, so the adapter can verify them with its public key.
const discordMockPrivPEM = `-----BEGIN PRIVATE KEY-----
MC4CAQAwBQYDK2VwBCIEIIdyw4XtKxfyuq1NMNaugUDCxnsuTTcv2rFQxI/KUXIu
-----END PRIVATE KEY-----
`

// TestDiscordStyleInteractionsVerify drives the Ed25519-signed interactions
// webhook: a valid signature → 200 (PONG for a ping), a bad one → 401.
func TestDiscordStyleInteractionsVerify(t *testing.T) {
	base, cleanup := bootDiscord(t)
	defer cleanup()

	// Parse the mock private key for signing (the adapter verifies with the
	// matching public key).
	block, _ := pem.Decode([]byte(discordMockPrivPEM))
	if block == nil {
		t.Fatal("decode priv PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse priv: %v", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		t.Fatal("not an ed25519 key")
	}

	ts := "1700000000"
	body := `{"type":1,"data":{}}`
	sig := hex.EncodeToString(ed25519.Sign(priv, []byte(ts+body)))

	post := func(sigHex string) int {
		req, _ := http.NewRequest("POST", base+"/interactions", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature-Ed25519", sigHex)
		req.Header.Set("X-Signature-Timestamp", ts)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post(sig); got != 200 {
		t.Fatalf("valid signature -> %d, want 200", got)
	}
	if got := post("deadbeef"); got != 401 {
		t.Fatalf("invalid signature -> %d, want 401", got)
	}
}
