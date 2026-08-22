package conformance

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twilio/twilio-go"
	tclient "github.com/twilio/twilio-go/client"
	v2010 "github.com/twilio/twilio-go/rest/api/v2010"
)

// twilioMockAuthToken mirrors the adapter's documented mock auth token —
// real-looking 32-hex, because official SDKs (twilio-go) client-side
// validate credentials as alphanumeric and reject underscore tokens.
const twilioMockAuthToken = "feed0000face1111beef2222cafe3333"

// TestTwilioSDKConformance drives the official twilio-go SDK against the
// twilio-style adapter with the documented test credentials: message
// create, the queued->sent->delivered lifecycle through SDK Fetch polling,
// the magic invalid-number failure trigger, list filters, and a signed
// status-callback webhook verified with Twilio's documented formula.
func TestTwilioSDKConformance(t *testing.T) {
	base := Boot(t, "twilio-style")

	const sid = "AC" + "0123456789abcdef0123456789abcdef"
	client := newTwilioClient(t, base, sid)

	// ===== Create + lifecycle (queued -> sent -> delivered via SDK fetch) =====

	msg, err := client.Api.CreateMessage(&v2010.CreateMessageParams{
		To:   ptr("+15550002222"),
		From: ptr("+15550001111"),
		Body: ptr("conformance hello"),
	})
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	msgSid := deref(msg.Sid)
	if !strings.HasPrefix(msgSid, "SM") {
		t.Fatalf("message Sid = %q, want SM prefix", msgSid)
	}
	Record(t, "twilio-go", "twilio-style", "CreateMessage (SM sid assigned)")

	var terminal string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, err := client.Api.FetchMessage(msgSid, nil)
		if err != nil {
			t.Fatalf("FetchMessage: %v", err)
		}
		if st := deref(m.Status); st == "delivered" || st == "failed" || st == "sent" {
			terminal = st
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if terminal != "delivered" && terminal != "sent" {
		t.Fatalf("lifecycle poll ended at %q, want sent/delivered (derive-on-read transitions)", terminal)
	}
	Record(t, "twilio-go", "twilio-style", "FetchMessage poll reaches terminal state")

	// ===== The magic invalid number -> failed =====

	bad, err := client.Api.CreateMessage(&v2010.CreateMessageParams{
		To:   ptr("+15005550001"),
		From: ptr("+15550001111"),
		Body: ptr("should fail"),
	})
	if err != nil {
		t.Fatalf("CreateMessage invalid number: %v", err)
	}
	badSid := deref(bad.Sid)
	badTerminal := ""
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, err := client.Api.FetchMessage(badSid, nil)
		if err != nil {
			t.Fatalf("FetchMessage bad: %v", err)
		}
		if st := deref(m.Status); st != "queued" && st != "" {
			badTerminal = st
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if badTerminal != "failed" {
		t.Fatalf("invalid number terminal status = %q, want failed", badTerminal)
	}
	Record(t, "twilio-go", "twilio-style", "+15005550001 magic number -> failed")

	// ===== List with a To filter (only the matching message) =====

	list, err := client.Api.ListMessage(&v2010.ListMessageParams{
		To: ptr("+15550002222"),
	})
	if err != nil {
		t.Fatalf("ListMessage: %v", err)
	}
	for _, m := range list {
		if deref(m.To) != "+15550002222" {
			t.Fatalf("filter leaked: message To=%q in a To=+15550002222 list", deref(m.To))
		}
	}
	if len(list) == 0 {
		t.Fatal("To filter returned nothing")
	}
	Record(t, "twilio-go", "twilio-style", "ListMessage To filter honored")

	// ===== Signed status-callback webhook (Twilio's documented formula) =====

	var mu sync.Mutex
	var callbacks []struct {
		url     string
		body    string
		headers http.Header
	}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		callbacks = append(callbacks, struct {
			url     string
			body    string
			headers http.Header
		}{"http://" + r.Host + r.URL.RequestURI(), string(b), r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()

	// The adapter signs deliveries against the engine-registered sink.
	cbBase := Boot(t, "twilio-style", sink.URL)
	cbClient := newTwilioClient(t, cbBase, sid)
	cbMsg, err := cbClient.Api.CreateMessage(&v2010.CreateMessageParams{
		To:   ptr("+15550003333"),
		From: ptr("+15550001111"),
		Body: ptr("with callback"),
	})
	if err != nil {
		t.Fatalf("CreateMessage callback: %v", err)
	}
	// Callbacks fire per status TRANSITION and transitions are read-driven
	// (derive-on-read) — poll this engine's message to delivered so BOTH
	// the sent and delivered callbacks fire.
	cbSid := deref(cbMsg.Sid)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, err := cbClient.Api.FetchMessage(cbSid, nil)
		if err != nil {
			t.Fatalf("FetchMessage cb: %v", err)
		}
		if st := deref(m.Status); st == "delivered" || st == "failed" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(callbacks)
		mu.Unlock()
		if n >= 2 { // sent + delivered hops
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(callbacks) == 0 {
		t.Fatal("no status callbacks arrived at the sink")
	}
	for _, cb := range callbacks {
		sig := cb.headers.Get("X-Twilio-Signature")
		if sig == "" {
			t.Fatal("callback without X-Twilio-Signature")
		}
		// base64(HMAC-SHA1(authToken, url + sorted form params)).
		vals, err := url.ParseQuery(cb.body)
		if err != nil {
			t.Fatalf("callback body not a form: %v", err)
		}
		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString(cb.url)
		for _, k := range keys {
			sb.WriteString(k + vals.Get(k))
		}
		mac := hmac.New(sha1.New, []byte(twilioMockAuthToken))
		mac.Write([]byte(sb.String()))
		want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if sig != want {
			t.Fatalf("X-Twilio-Signature mismatch:\n got %q\nwant %q (url %s)", sig, want, cb.url)
		}
	}
	Record(t, "twilio-go", "twilio-style", "status callbacks verify against Twilio's HMAC-SHA1 formula")
}

// newTwilioClient builds the official client on the rewrite transport —
// the SDK hardcodes api.twilio.com, and accepts a custom BaseClient.
func newTwilioClient(t *testing.T, base, sid string) *twilio.RestClient {
	t.Helper()
	bc := &tclient.Client{
		Credentials: tclient.NewCredentials(sid, twilioMockAuthToken),
		HTTPClient:  RewriteClient(t, base),
	}
	bc.SetAccountSid(sid)
	return twilio.NewRestClientWithParams(twilio.ClientParams{Client: bc})
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
