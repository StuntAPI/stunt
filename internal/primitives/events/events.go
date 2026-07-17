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
	mu        sync.RWMutex
	targets   map[string]string
	client    *http.Client
	maxRetries int
	backoff   time.Duration
}

// envelope is the JSON body sent to webhook targets.
type envelope struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
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
// returned). Not safe for concurrent use with Emit; call before emitting.
func (e *Emitter) SetMaxRetries(n int) {
	if n < 1 {
		n = 1
	}
	e.maxRetries = n
}

// Register sets the webhook target URL for the given namespace, overwriting
// any previous registration.
func (e *Emitter) Register(ns, url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.targets[ns] = url
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
// On non-2xx response or transport error, the request is retried up to
// maxRetries times with exponential-ish backoff. Returns the last error
// if all attempts fail.
func (e *Emitter) Emit(ctx context.Context, ns, eventType string, payload map[string]any) error {
	e.mu.RLock()
	url, ok := e.targets[ns]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("events: emit %s/%s: %w", ns, eventType, ErrNotRegistered)
	}

	body, err := json.Marshal(envelope{Type: eventType, Payload: payload})
	if err != nil {
		return fmt.Errorf("events: marshal envelope: %w", err)
	}

	// Defensive: ensure at least one attempt even if maxRetries was set to
	// zero or negative externally.
	maxRetries := e.maxRetries
	if maxRetries < 1 {
		maxRetries = 1
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(e.backoff << (attempt - 1)):
			}
		}

		lastErr = e.doPost(ctx, url, body)
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
func (e *Emitter) doPost(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

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
