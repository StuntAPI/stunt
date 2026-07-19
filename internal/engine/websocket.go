package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/coder/websocket"
	"github.com/stunt-adapters/stunt/internal/adapter"
	"github.com/stunt-adapters/stunt/internal/starlark"
	sk "go.starlark.net/starlark"
)

// wsMaxSteps is the elevated step budget for WebSocket connection-lifetime
// handlers. The default (1M) is too tight for legitimately chatty connections
// that exchange many messages, each with moderate per-message work. The
// key property is that ws.recv() is a blocking builtin: while it blocks
// waiting for the next message, zero steps accrue. Therefore:
//
//   - Idle / slow long-lived connections live indefinitely (recv blocks, 0 steps).
//   - A handler that tight-loops doing ws.send(...) without ever recv()-ing
//     is still killed by this limit (correct DoS guard).
//
// 10M steps is 10x the default; sufficient for thousands of message exchanges
// without prematurely terminating legitimate connections.
const wsMaxSteps = 10_000_000

// wsMaxConcurrentConns is the maximum number of concurrent active WebSocket
// connections per engine. Once this limit is reached, new upgrade attempts
// are rejected with HTTP 503 (Service Unavailable) so clients get a clear
// signal to back off. This prevents a single adapter from exhausting file
// descriptors, goroutines, or memory under load.
const wsMaxConcurrentConns = 256

// isWebSocketUpgrade reports whether the HTTP request is a WebSocket upgrade
// request. It checks for the required Upgrade and Connection headers per
// RFC 6455 §4.1. coder/websocket's Accept does its own validation; this is
// a fast pre-check to avoid attempting an upgrade on regular HTTP requests.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		containsToken(r.Header.Get("Connection"), "upgrade")
}

// containsToken reports whether val contains the token (case-insensitive).
func containsToken(val, token string) bool {
	for _, part := range strings.Split(val, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// handleWebsocket upgrades the HTTP connection to a WebSocket and runs the
// connection-lifetime Starlark handler. It blocks the ServeHTTP goroutine
// until the handler returns (client disconnect, graceful close, or engine
// shutdown). Panic recovery ensures a buggy handler never crashes the
// server.
//
// The handler signature is: on_connect(ws). The ws object exposes recv(),
// send(), and close() methods. When the client disconnects, recv() returns
// None and the handler returns naturally.
//
// Concurrent connections are capped by the engine's wsSem semaphore to
// prevent resource exhaustion. If the cap is reached, the upgrade is
// rejected with HTTP 503.
//
// On engine shutdown, the handler proactively sends a StatusGoingAway close
// frame to the client via the engine's shutdownCh, so the client sees a
// clean WebSocket close rather than a bare TCP teardown. Go's
// http.Server.Shutdown does not cancel request contexts for hijacked
// (WebSocket) connections, so the shutdownCh is the only signal.
func (e *Engine) handleWebsocket(
	w http.ResponseWriter,
	r *http.Request,
	st *serviceState,
	ep adapter.WebsocketEndpoint,
) {
	// Recover from panics inside the Starlark VM or websocket layer so a
	// buggy handler never crashes the HTTP server.
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "engine: websocket handler panic: %v\n", rec)
		}
	}()

	// Acquire a semaphore slot BEFORE accepting the upgrade. If the cap is
	// reached, reject with 503 so clients get a clear signal to back off.
	select {
	case e.wsSem <- struct{}{}:
		defer func() { <-e.wsSem }()
	default:
		http.Error(w, "too many concurrent WebSocket connections", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: ep.Subprotocols,
	})
	if err != nil {
		// Accept already wrote an error response via w.
		return
	}

	// Use an independent context for WebSocket I/O rather than r.Context().
	//
	// coder/websocket registers a context.AfterFunc in setupReadTimeout that
	// calls c.close() (raw TCP teardown) as soon as the read context is
	// cancelled. If we passed r.Context(), a server shutdown would tear down
	// the TCP connection before we could write a close frame. By using an
	// independent context we keep the connection alive until WE decide to
	// close it.
	wsCtx, wsCancel := context.WithCancel(context.Background())
	defer wsCancel()

	// Monitor for engine shutdown. When the engine's shutdownCh is closed,
	// proactively close the connection with StatusGoingAway so the client
	// receives a clean close frame.
	//
	// Go's http.Server.Shutdown removes hijacked connections from its
	// activeConn map (StateHijacked), so closeIdleConns returns immediately
	// without waiting for the handler to finish. r.Context() is never
	// cancelled for hijacked connections, so the shutdownCh is the only
	// reliable signal.
	go func() {
		select {
		case <-e.shutdownCh:
			_ = conn.Close(websocket.StatusGoingAway, "")
		case <-wsCtx.Done():
			// Handler returned normally; nothing to do.
		}
	}()

	// Defer a close for when the handler returns (client disconnect, handler
	// error, etc.). conn.Close is idempotent, so if the shutdown goroutine
	// already closed the connection, this is a no-op.
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	scriptPath, fnName := adapter.SplitHandler(ep.Handler)

	vm, err := st.getOrLoadVM(scriptPath)
	if err != nil {
		// Can't run the handler; the close is handled by the defer above.
		return
	}

	wsv := &wsValue{conn: conn, ctx: wsCtx}

	// CallWithMaxSteps uses an elevated step budget (wsMaxSteps) so chatty
	// connections are not prematurely killed. The blocking recv() accrues
	// no steps while waiting, so idle connections live indefinitely.
	_, _ = vm.CallWithMaxSteps(fnName, wsv, wsMaxSteps)
}

// wsValue is a Starlark value that exposes recv(), send(), and close()
// methods to a WebSocket connection-lifetime handler script. It wraps a
// *websocket.Conn, converting between Go values and Starlark values.
//
// In Starlark:
//
//	msg = ws.recv()            # dict (JSON), str, or None (client closed)
//	ws.send({"k": v})          # send a JSON text frame
//	ws.send([1, 2, 3])         # send a JSON array text frame
//	ws.send("hello")           # send a text frame
//	ws.close(code=1000, reason="")  # graceful close
type wsValue struct {
	conn *websocket.Conn
	ctx  context.Context
}

// String implements sk.Value.
func (w *wsValue) String() string { return "ws" }

// Type implements sk.Value.
func (w *wsValue) Type() string { return "ws" }

// Freeze implements sk.Value. The ws object is inherently per-connection and
// not frozen — handlers operate on it within a single invocation.
func (w *wsValue) Freeze() {}

// Truth implements sk.Value.
func (w *wsValue) Truth() sk.Bool { return true }

// Hash implements sk.Value.
func (w *wsValue) Hash() (uint32, error) { return 0, fmt.Errorf("ws is unhashable") }

// Attr implements sk.HasAttrs, returning the recv, send, and close builtins.
func (w *wsValue) Attr(name string) (sk.Value, error) {
	switch name {
	case "recv":
		return sk.NewBuiltin("recv", w.recv), nil
	case "send":
		return sk.NewBuiltin("send", w.send), nil
	case "close":
		return sk.NewBuiltin("close", w.close), nil
	default:
		return nil, nil // no such attribute
	}
}

// AttrNames implements sk.HasAttrs.
func (w *wsValue) AttrNames() []string {
	return []string{"recv", "send", "close"}
}

// recv reads the next WebSocket message. It returns a Starlark dict if the
// message is valid JSON (object), a str for other text/binary messages, or
// None when the client has closed the connection (clean EOF). Errors from
// context cancellation propagate as Starlark errors.
//
// This is a BLOCKING builtin: while waiting for the next message, no Starlark
// VM steps accrue. This means idle connections are not killed by the step
// limit.
//
// On engine shutdown the wsValue uses an independent context, so recv will
// not return until the connection is actually closed (by the shutdown
// goroutine or by the client). See handleWebsocket for details.
func (w *wsValue) recv(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	if err := sk.UnpackArgs("recv", args, kwargs); err != nil {
		return nil, err
	}

	msgType, reader, err := w.conn.Reader(w.ctx)
	if err != nil {
		// Clean close or EOF → return None so the handler can break its loop.
		if errors.Is(err, io.EOF) ||
			errors.Is(err, io.ErrUnexpectedEOF) ||
			websocket.CloseStatus(err) != -1 {
			return sk.None, nil
		}
		// Context cancellation or other error → propagate as Starlark error
		// so the handler terminates.
		return nil, err
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		if errors.Is(err, io.EOF) ||
			errors.Is(err, io.ErrUnexpectedEOF) ||
			websocket.CloseStatus(err) != -1 {
			return sk.None, nil
		}
		return nil, err
	}

	// Binary messages are returned as str (Starlark has no first-class bytes).
	if msgType == websocket.MessageBinary {
		return sk.String(string(data)), nil
	}

	// Text message: try to parse as JSON object; if that succeeds, return as
	// dict. Otherwise return as plain str.
	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil {
		sv, err := starlark.GoToStarlark(obj)
		if err != nil {
			return nil, fmt.Errorf("recv: %w", err)
		}
		return sv, nil
	}

	return sk.String(string(data)), nil
}

// send writes an outbound message. A dict argument is marshalled to a JSON
// text frame; a list argument is marshalled to a JSON array; scalar values
// (int, float, bool) are sent as their JSON representation; a str argument is
// sent as a raw text frame. Returns None on success.
func (w *wsValue) send(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var msgVal sk.Value
	if err := sk.UnpackArgs("send", args, kwargs, "msg", &msgVal); err != nil {
		return nil, err
	}

	var payload []byte
	switch v := msgVal.(type) {
	case *sk.Dict:
		m, err := starlark.StarlarkToGo(v)
		if err != nil {
			return nil, fmt.Errorf("send: convert dict: %w", err)
		}
		data, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("send: marshal dict to JSON: %w", err)
		}
		payload = data
	case *sk.List:
		l, err := starlark.StarlarkListToGo(v)
		if err != nil {
			return nil, fmt.Errorf("send: convert list: %w", err)
		}
		data, err := json.Marshal(l)
		if err != nil {
			return nil, fmt.Errorf("send: marshal list to JSON: %w", err)
		}
		payload = data
	case sk.Int:
		n, _ := v.Int64()
		data, err := json.Marshal(n)
		if err != nil {
			return nil, fmt.Errorf("send: marshal int to JSON: %w", err)
		}
		payload = data
	case sk.Float:
		data, err := json.Marshal(float64(v))
		if err != nil {
			return nil, fmt.Errorf("send: marshal float to JSON: %w", err)
		}
		payload = data
	case sk.Bool:
		data, err := json.Marshal(bool(v))
		if err != nil {
			return nil, fmt.Errorf("send: marshal bool to JSON: %w", err)
		}
		payload = data
	default:
		// Any other value: stringify.
		s, ok := sk.AsString(msgVal)
		if !ok {
			s = msgVal.String()
		}
		payload = []byte(s)
	}

	if err := w.conn.Write(w.ctx, websocket.MessageText, payload); err != nil {
		return nil, err
	}
	return sk.None, nil
}

// validateCloseCode checks whether a status code is valid for use in a
// WebSocket close frame per RFC 6455 §7.4. The valid ranges are 1000–1014
// (excluding reserved codes 1004, 1005, 1006, 1015) and 3000–4999
// (application-defined).
func validateCloseCode(code int) error {
	switch websocket.StatusCode(code) {
	case 1004, 1005, 1006, 1015:
		return fmt.Errorf("close: status code %d is reserved and cannot be used", code)
	}
	if code >= 1000 && code <= 1014 {
		return nil
	}
	if code >= 3000 && code <= 4999 {
		return nil
	}
	return fmt.Errorf("close: status code %d is not a valid WebSocket close code", code)
}

// close performs a graceful WebSocket close. The code defaults to 1000
// (StatusNormalClosure) and the reason defaults to empty. After close, the
// connection is terminated. Invalid status codes produce a Starlark error
// rather than sending a malformed close frame.
func (w *wsValue) close(_ *sk.Thread, _ *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var codeVal sk.Value
	var reasonVal sk.Value
	if err := sk.UnpackArgs("close", args, kwargs, "code?", &codeVal, "reason?", &reasonVal); err != nil {
		return nil, err
	}

	code := websocket.StatusNormalClosure
	if codeVal != nil {
		if c, ok := codeVal.(sk.Int); ok {
			n, _ := c.Int64()
			if err := validateCloseCode(int(n)); err != nil {
				return nil, err
			}
			code = websocket.StatusCode(n)
		}
	}

	reason := ""
	if reasonVal != nil {
		if s, ok := sk.AsString(reasonVal); ok {
			reason = s
		}
	}

	_ = w.conn.Close(code, reason)
	return sk.None, nil
}
