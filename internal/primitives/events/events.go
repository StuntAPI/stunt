// Package events provides a webhook emitter for pushing event payloads to
// registered target URLs via HTTP POST. Each service namespace can have
// one registered webhook target. Emissions retry on failure with backoff.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrNotRegistered is returned when Emit is called for a namespace that has
// no registered webhook target.
var ErrNotRegistered = errors.New("events: namespace not registered")

// Default retry settings.
const (
	defaultMaxRetries = 3
	defaultTimeout    = 10 * time.Second
	defaultBackoff    = 100 * time.Millisecond
)

// Emitter manages webhook target registrations and event delivery.
// It is safe for concurrent use by multiple goroutines.
type Emitter struct {
	mu         sync.RWMutex
	targets    map[string]string
	client     *http.Client
	maxRetries int
	backoff    time.Duration
}

// envelope is the JSON body sent to webhook targets.
type envelope struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// MarshalEnvelope returns the exact JSON body Emit POSTs for (eventType,
// payload). It is the single source of truth for the delivery body: the
// events_body Starlark builtin calls it, so a signature computed over
// events_body(...) output verifies against the bytes the sink receives.
//
// encoding/json with default settings is deterministic (struct fields in tag
// order, map[string]any keys sorted, <,>,& HTML-escaped). Signers must MAC
// these bytes verbatim, never a re-marshalled copy.
func MarshalEnvelope(eventType string, payload map[string]any) ([]byte, error) {
	return json.Marshal(envelope{Type: eventType, Payload: payload})
}

// NewEmitter creates an Emitter with default retry settings.
func NewEmitter() *Emitter {
	return &Emitter{
		targets:    make(map[string]string),
		client:     &http.Client{Timeout: defaultTimeout},
		maxRetries: defaultMaxRetries,
		backoff:    defaultBackoff,
	}
}

// SetMaxRetries configures the maximum number of delivery attempts. Values
// less than 1 are clamped to 1 so that Emit always makes at least one attempt
// (otherwise the retry loop is skipped and a confusing nil-wrapped error is
// returned). Safe for concurrent use with Emit.
func (e *Emitter) SetMaxRetries(n int) {
	if n < 1 {
		n = 1
	}
	e.mu.Lock()
	e.maxRetries = n
	e.mu.Unlock()
}

// SetBackoff configures the base retry backoff duration. The actual delay
// for attempt N is backoff << (N-1) (exponential). Safe for concurrent use
// with Emit.
func (e *Emitter) SetBackoff(d time.Duration) {
	e.mu.Lock()
	e.backoff = d
	e.mu.Unlock()
}

// Register sets the webhook target URL for the given namespace, overwriting
// any previous registration.
func (e *Emitter) Register(ns, url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.targets[ns] = url
}

// Target returns the currently-registered webhook URL for the given namespace,
// or "" if none is registered. Handlers use it to learn where events_emit will
// deliver (needed by providers whose signature MACs the destination URL, e.g.
// Twilio and Square).
func (e *Emitter) Target(ns string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.targets[ns]
}

// Close releases resources associated with the Emitter, including closing
// idle HTTP connections from the underlying client's pool. After Close, the
// Emitter should not be used.
func (e *Emitter) Close() {
	e.client.CloseIdleConnections()
}

// Emit sends an event of the given type with the given payload to the
// registered webhook target for ns. The body is a JSON envelope:
//
//	{"type": "<eventType>", "payload": { ... }}
//
// headers are applied to the delivery on top of the default
// Content-Type: application/json (a caller Content-Type overrides it). A nil
// map adds nothing. Unsafe headers — reserved names (Host, Content-Length,
// Transfer-Encoding, …) and any CR/LF or Unicode line separators — are
// rejected by validateHeader before the request is sent, to prevent
// header/request smuggling.
//
// On non-2xx response or transport error, the request is retried up to
// maxRetries times with exponential-ish backoff. Returns the last error
// if all attempts fail.
func (e *Emitter) Emit(ctx context.Context, ns, eventType string, payload map[string]any, headers map[string]string) error {
	body, err := MarshalEnvelope(eventType, payload)
	if err != nil {
		return fmt.Errorf("events: marshal envelope: %w", err)
	}
	return e.EmitRaw(ctx, ns, eventType, body, headers)
}

// EmitRaw delivers an exact pre-marshaled body to the service's registered
// target. Providers whose webhook receivers parse the delivery as the
// provider's own event object (Stripe, GitHub, …) need the real shape on the
// wire — not the {type, payload} envelope — so signature schemes that MAC the
// raw bytes and SDK event parsers both verify against what the sink receives.
// eventType names the event for registration bookkeeping only; it does not
// appear in the body. Retry/header/validation semantics match Emit.
func (e *Emitter) EmitRaw(ctx context.Context, ns, eventType string, body []byte, headers map[string]string) error {
	e.mu.RLock()
	url, ok := e.targets[ns]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("events: emit %s/%s: %w", ns, eventType, ErrNotRegistered)
	}

	// Fail fast on bad caller headers: these are permanent errors (a malformed
	// header or a reserved name), not transient delivery failures, so they
	// must short-circuit before the retry loop sends anything.
	for k, v := range headers {
		if err := validateHeader(k, v); err != nil {
			return err
		}
	}

	// Read retry settings under the read-lock to avoid a data race with
	// SetMaxRetries/SetBackoff.
	e.mu.RLock()
	maxRetries := e.maxRetries
	backoff := e.backoff
	e.mu.RUnlock()

	// Defensive: ensure at least one attempt even if maxRetries was set to
	// zero or negative externally.
	if maxRetries < 1 {
		maxRetries = 1
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff << (attempt - 1)):
			}
		}

		lastErr = e.doPost(ctx, url, body, headers)
		if lastErr == nil {
			return nil
		}

		// Don't retry if the context is already cancelled.
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return lastErr
		}
	}
	return fmt.Errorf("events: emit %s/%s failed after %d attempts: %w", ns, eventType, maxRetries, lastErr)
}

// doPost performs a single HTTP POST and returns nil on 2xx, an error otherwise.
func (e *Emitter) doPost(ctx context.Context, url string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Cap the response body read to avoid unbounded memory use from a
	// misbehaving or malicious server.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return nil
}

// validateHeader rejects caller-supplied headers that could hijack the
// request — header/request smuggling prevention:
//   - empty key, or a key with CTL/space/NUL/DEL (not an RFC 7230 token);
//   - a value with CTL (except tab), NUL, DEL, CR, or LF;
//   - a value with a Unicode line separator (U+0085/U+2028/U+2029) — these
//     slip past Go's transport and are recognized as request terminators by
//     some frontends;
//   - reserved names the transport owns (Host, Content-Length,
//     Transfer-Encoding, Trailer, Connection).
//
// It is a permanent error: Emit checks it once before the retry loop, so a
// bad header fails fast and never reaches the wire.
func validateHeader(key, val string) error {
	if key == "" {
		return fmt.Errorf("events: invalid header: empty key")
	}
	for _, b := range []byte(key) {
		if b <= 0x20 || b == 0x7f {
			return fmt.Errorf("events: invalid header key %q", key)
		}
	}
	for _, b := range []byte(val) {
		if (b < 0x20 && b != '\t') || b == 0x7f {
			return fmt.Errorf("events: invalid header value for %q", key)
		}
	}
	if strings.ContainsAny(val, "  ") {
		return fmt.Errorf("events: invalid header value for %q: line separator not allowed", key)
	}
	for _, reserved := range []string{"Host", "Content-Length", "Transfer-Encoding", "Trailer", "Connection"} {
		if strings.EqualFold(key, reserved) {
			return fmt.Errorf("events: cannot set reserved header %q", key)
		}
	}
	return nil
}
