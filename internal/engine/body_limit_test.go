package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// echoAdapterYAML is a minimal adapter whose single handler echoes the raw
// request body back verbatim, so tests can assert byte-exact round-trips.
const echoAdapterYAML = `
id: echo
name: Echo
endpoints:
  - route: /echo
    method: POST
    handler: scripts/echo.star#on_echo
`

const echoStar = `
def on_echo(req):
    return respond(200, req["raw_body"], {"Content-Type": "application/octet-stream"})
`

// bootEchoService builds an in-test echo adapter, boots an engine with the
// given per-service max_body_bytes (0 = default), and returns the base URL.
func bootEchoService(t *testing.T, maxBodyBytes int64) string {
	t.Helper()

	adapterDir := t.TempDir()
	writeFile(t, adapterDir, "adapter.yaml", echoAdapterYAML)
	writeFile(t, adapterDir, "scripts/echo.star", echoStar)

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    stateDir + "/stunt.yaml",
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"echo": {Adapter: adapterDir, MaxBodyBytes: maxBodyBytes},
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

	return addrs["echo"]
}

// postBytes POSTs raw bytes and returns the response status and body.
func postBytes(t *testing.T, url string, payload []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// allByteValues returns a payload that contains every possible byte value
// (0x00-0xFF), prefixed with the PNG magic so it is definitely not valid
// UTF-8 or JSON. Any sanitization anywhere in the pipeline breaks the
// byte-equality assertions loudly.
func allByteValues(prefixLen int) []byte {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	out := append([]byte{}, png...)
	for i := 0; i < 256; i++ {
		out = append(out, byte(i))
	}
	for len(out) < prefixLen {
		out = append(out, byte(len(out)%251))
	}
	return out
}

// TestBodyLimitDefaultOversizeIs413 proves the engine no longer silently
// truncates bodies over the default 1 MiB limit: the request must fail with
// 413, and the handler must never see truncated data.
func TestBodyLimitDefaultOversizeIs413(t *testing.T) {
	base := bootEchoService(t, 0)

	payload := bytes.Repeat([]byte{0xAB}, 1<<20+1) // 1 MiB + 1
	status, _ := postBytes(t, base+"/echo", payload)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body -> status %d, want 413", status)
	}
}

// TestBodyLimitDefaultUnderLimitRoundTrips proves a body just under the
// default limit round-trips byte-exact (no truncation, no mangling).
func TestBodyLimitDefaultUnderLimitRoundTrips(t *testing.T) {
	base := bootEchoService(t, 0)

	payload := allByteValues(1<<20 - 1) // 1 byte under the default limit
	status, body := postBytes(t, base+"/echo", payload)
	if status != 200 {
		t.Fatalf("under-limit body -> status %d, want 200", status)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("echoed body differs from sent body: got %d bytes, want %d bytes (byte-exact)", len(body), len(payload))
	}
}

// TestBodyLimitConfigurable proves max_body_bytes raises and lowers the
// per-service limit.
func TestBodyLimitConfigurable(t *testing.T) {
	t.Run("small limit rejects oversize", func(t *testing.T) {
		base := bootEchoService(t, 128)

		status, _ := postBytes(t, base+"/echo", bytes.Repeat([]byte{0x01}, 129))
		if status != http.StatusRequestEntityTooLarge {
			t.Fatalf("129 bytes with 128-byte limit -> status %d, want 413", status)
		}

		payload := bytes.Repeat([]byte{0x02}, 128)
		status, body := postBytes(t, base+"/echo", payload)
		if status != 200 {
			t.Fatalf("128 bytes with 128-byte limit -> status %d, want 200", status)
		}
		if !bytes.Equal(body, payload) {
			t.Fatalf("at-limit body did not round-trip byte-exact")
		}
	})

	t.Run("raised limit accepts multi-MiB body", func(t *testing.T) {
		base := bootEchoService(t, 8<<20)

		payload := allByteValues(3 << 20) // 3 MiB, over the old 1 MiB default
		status, body := postBytes(t, base+"/echo", payload)
		if status != 200 {
			t.Fatalf("3 MiB body with 8 MiB limit -> status %d, want 200", status)
		}
		if !bytes.Equal(body, payload) {
			t.Fatalf("3 MiB body did not round-trip byte-exact: got %d bytes, want %d", len(body), len(payload))
		}
	})
}
