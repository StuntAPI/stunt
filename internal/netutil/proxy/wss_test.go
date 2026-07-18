package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stunt-adapters/stunt/internal/netutil"
)

// startWSBackend starts a plain HTTP backend that upgrades to WebSocket and
// echoes every received message back to the client. It returns the dialable
// backend address (127.0.0.1:PORT). Registers cleanup with t.
func startWSBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(wsEchoHandler))
	t.Cleanup(srv.Close)
	u := strings.TrimPrefix(srv.URL, "http://")
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatalf("SplitHostPort(%s): %v", u, err)
	}
	return net.JoinHostPort(host, port)
}

// wsEchoHandler is an HTTP handler that accepts a WebSocket upgrade and
// echoes each inbound message back to the client. It represents the kind of
// backend the stunt engine exposes (a WS endpoint).
func wsEchoHandler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	ctx := r.Context()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if err := c.Write(ctx, websocket.MessageText, data); err != nil {
			return
		}
	}
}

// wssDial connects a WebSocket client to the proxy over TLS (WSS), trusting
// the proxy's CA and presenting the given SNI server name + Host header, but
// dialing the proxy's actual TCP address (bypassing DNS so the test is
// host-safe). Returns an open connection.
func wssDial(t *testing.T, ca *netutil.CA, proxyAddr, serverName, path string) *websocket.Conn {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca.CertPEM))

	// Custom transport: ignore the URL-derived address and always dial the
	// proxy's real address, while presenting the chosen SNI server name and
	// trusting the minted leaf cert.
	dialTLS := func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}
		raw, err := d.DialContext(ctx, network, proxyAddr)
		if err != nil {
			return nil, err
		}
		tlsCfg := &tls.Config{
			RootCAs:    pool,
			ServerName: serverName,
			// WebSocket is HTTP/1.1; negotiate only http/1.1 so the upgrade
			// hijack path engages (RFC 8441 extended-CONNECT is unsupported
			// by the standard library). This mirrors real WS clients.
			NextProtos: []string{"http/1.1"},
		}
		return tls.Client(raw, tlsCfg), nil
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialTLSContext: dialTLS,
		},
	}

	url := "wss://" + serverName + path
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: client,
		Host:       serverName, // Host header drives proxy routing
	})
	if err != nil {
		t.Fatalf("wss dial %s: %v", url, err)
	}
	return c
}

// TestProxyWSS_EchoRoundTrip verifies that the TLS proxy forwards WebSocket
// upgrade requests end-to-end (WSS): a real WSS client connects through the
// proxy, which terminates TLS and reverse-proxies the HTTP/1.1 Upgrade to the
// plaintext backend; messages must round-trip.
//
// This is the portless.dev topology for WebSocket: the real path users take
// is wss://<host>.<tld>/ — so this guards against regressions in proxy
// upgrade handling.
func TestProxyWSS_EchoRoundTrip(t *testing.T) {
	ca := newTestCA(t)
	backend := startWSBackend(t)

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		CA:   ca,
		Backends: map[string]string{
			"echo.localhost": backend,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	conn := wssDial(t, ca, p.opts.Addr, "echo.localhost", "/ws")
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for _, msg := range []string{"hello", "world", "123"} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		_, got, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read after %q: %v", msg, err)
		}
		if string(got) != msg {
			t.Fatalf("echo mismatch: sent %q got %q", msg, string(got))
		}
	}
}

// TestProxyWSS_JSONRoundTrip verifies JSON (dict) messages round-trip through
// the WSS proxy path.
func TestProxyWSS_JSONRoundTrip(t *testing.T) {
	ca := newTestCA(t)
	backend := startWSBackend(t)

	p, err := New(Options{
		TLS:  true,
		Addr: freeAddr(t),
		CA:   ca,
		Backends: map[string]string{
			"chat.localhost": backend,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startProxy(t, p)

	conn := wssDial(t, ca, p.opts.Addr, "chat.localhost", "/ws")
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	payload := []byte(`{"type":"ping","n":42}`)
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, got, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo mismatch: got %q want %q", string(got), string(payload))
	}
}
