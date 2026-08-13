package engine

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// notify captures one webhook delivery's raw body and headers.
type notify struct {
	body []byte
	hdr  http.Header
}

// captureSink is an httptest sink that retains the raw body + headers of every
// delivery. Signature verifiers MAC the raw bytes (the exact wire body the sink
// received), not a re-marshalled JSON object — key-order drift would spuriously
// break the MAC.
type captureSink struct {
	srv      *httptest.Server
	mu       sync.Mutex
	notifies []notify
}

func newCaptureSink() *captureSink {
	s := &captureSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.notifies = append(s.notifies, notify{body: b, hdr: r.Header.Clone()})
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return s
}

func (s *captureSink) close() { s.srv.Close() }

// awaitDelivery polls until a delivery arrives or the timeout elapses, then
// returns its raw body + headers. Fails the test if nothing arrives.
func (s *captureSink) awaitDelivery(t *testing.T, timeout time.Duration) ([]byte, http.Header) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.notifies)
		s.mu.Unlock()
		if n > 0 {
			s.mu.Lock()
			defer s.mu.Unlock()
			last := s.notifies[n-1]
			return last.body, last.hdr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("webhook sink received no delivery")
	return nil, nil
}

// verifyStripeSig re-derives the Stripe v1 signature over "{t}.{rawBody}" and
// compares it to the Stripe-Signature header. Mirrors the real Stripe formula.
func verifyStripeSig(t *testing.T, raw []byte, hdr http.Header, secret string) {
	t.Helper()
	sigHeader := hdr.Get("Stripe-Signature")
	var ts int64
	var v1 string
	for _, part := range strings.Split(sigHeader, ",") {
		switch {
		case strings.HasPrefix(part, "t="):
			ts, _ = strconv.ParseInt(part[2:], 10, 64)
		case strings.HasPrefix(part, "v1="):
			v1 = part[3:]
		}
	}
	if ts == 0 || v1 == "" {
		t.Fatalf("malformed Stripe-Signature %q", sigHeader)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts, raw)))
	want := hex.EncodeToString(mac.Sum(nil))
	if v1 != want {
		t.Fatalf("Stripe v1 mismatch: header=%s want=%s", v1, want)
	}
}

// verifyGitHubSig re-derives "sha256="+hex(HMAC-SHA256(secret, rawBody)) and
// compares it to the X-Hub-Signature-256 header. Mirrors the real GitHub formula.
func verifyGitHubSig(t *testing.T, raw []byte, hdr http.Header, secret, event string) {
	t.Helper()
	if got := hdr.Get("X-GitHub-Event"); got != event {
		t.Fatalf("X-GitHub-Event = %q, want %q", got, event)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := hdr.Get("X-Hub-Signature-256"); got != want {
		t.Fatalf("X-Hub-Signature-256 = %q, want %q", got, want)
	}
}

// verifyShopifySig re-derives base64(HMAC-SHA256(secret, rawBody)) and compares
// it to the X-Shopify-Hmac-SHA256 header. Shopify uses base64, NOT hex.
func verifyShopifySig(t *testing.T, raw []byte, hdr http.Header, secret string) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got := hdr.Get("X-Shopify-Hmac-SHA256"); got != want {
		t.Fatalf("X-Shopify-Hmac-SHA256 = %q, want %q", got, want)
	}
}
