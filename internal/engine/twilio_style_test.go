package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// Twilio synthetic test credentials (must match scripts/lib.star).
const (
	twilioAccountSID = "AC0123456789abcdef0123456789abcdef"
	twilioAuthToken  = "twilio_auth_token"
)

// twilioBasicAuth returns the value for an HTTP Basic Authorization header.
func twilioBasicAuth(sid, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(sid+":"+token))
}

// twilioPostJSON performs an authenticated JSON POST and returns body + status.
func twilioPostJSON(t *testing.T, url string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", twilioBasicAuth(twilioAccountSID, twilioAuthToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// twilioGet performs an authenticated GET and returns body + status.
func twilioGet(t *testing.T, url string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", twilioBasicAuth(twilioAccountSID, twilioAuthToken))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// twilioGetNoAuth performs a GET without any Authorization header.
func twilioGetNoAuth(t *testing.T, url string) (string, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// TestTwilioStyleAdapter exercises the Twilio-style adapter end-to-end:
//
//   - POST message → 201 with sid SM..., status queued
//   - GET message list → shows the sent message (STATEFUL)
//   - GET message by sid → shows persisted message
//   - Async lifecycle → queued derives to delivered after 3s;
//     simulate_fail send derives to undelivered
//   - POST call → 201 with sid CA..., status queued
//   - Verify create → pending; check with wrong code → pending; check with
//     right code → approved
//   - 401 without Basic auth
func TestTwilioStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "twilio-style")
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
			"twilio": {Adapter: absAdapterDir},
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

	base := addrs["twilio"]
	const accountSID = twilioAccountSID
	msgPath := base + "/2010-06-01/Accounts/" + accountSID + "/Messages.json"

	// ===== POST message → 201, sid SM..., status queued =====

	body, status := twilioPostJSON(t, msgPath, map[string]any{
		"To":   "+15551234567",
		"From": "+15557654321",
		"Body": "Hello from stunt!",
	})
	if status != 201 {
		t.Fatalf("POST message -> status %d, want 201; body %s", status, body)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("unmarshal message: %v (body %s)", err, body)
	}
	msgSID, ok := msg["sid"].(string)
	if !ok || !strings.HasPrefix(msgSID, "SM") {
		t.Fatalf("message sid = %v, want SM* prefix", msg["sid"])
	}
	if msg["status"] != "queued" {
		t.Fatalf("message status = %v, want queued", msg["status"])
	}
	if msg["body"] != "Hello from stunt!" {
		t.Fatalf("message body = %v, want 'Hello from stunt!'", msg["body"])
	}
	if msg["direction"] != "outbound-api" {
		t.Fatalf("message direction = %v, want outbound-api", msg["direction"])
	}
	if msg["api_version"] != "2010-06-01" {
		t.Fatalf("message api_version = %v, want 2010-06-01", msg["api_version"])
	}

	// ===== GET message list → shows the sent message (STATEFUL) =====

	body, status = twilioGet(t, msgPath)
	if status != 200 {
		t.Fatalf("GET messages -> status %d, want 200; body %s", status, body)
	}
	var msgList map[string]any
	if err := json.Unmarshal([]byte(body), &msgList); err != nil {
		t.Fatalf("unmarshal message list: %v (body %s)", err, body)
	}
	messages, ok := msgList["messages"].([]any)
	if !ok || len(messages) < 1 {
		t.Fatalf("messages = %v, want at least 1 item", msgList["messages"])
	}
	foundSent := false
	for _, m := range messages {
		mm := m.(map[string]any)
		if mm["sid"] == msgSID {
			foundSent = true
			if mm["body"] != "Hello from stunt!" {
				t.Fatalf("listed message body = %v, want 'Hello from stunt!'", mm["body"])
			}
		}
	}
	if !foundSent {
		t.Fatalf("sent message %s not found in message list", msgSID)
	}

	// ===== GET message by SID → persisted message =====

	body, status = twilioGet(t, base+"/2010-06-01/Accounts/"+accountSID+"/Messages/"+msgSID+".json")
	if status != 200 {
		t.Fatalf("GET message by sid -> status %d, want 200; body %s", status, body)
	}
	var retrieved map[string]any
	if err := json.Unmarshal([]byte(body), &retrieved); err != nil {
		t.Fatalf("unmarshal retrieved message: %v (body %s)", err, body)
	}
	if retrieved["sid"] != msgSID {
		t.Fatalf("retrieved sid = %v, want %v", retrieved["sid"], msgSID)
	}
	if retrieved["body"] != "Hello from stunt!" {
		t.Fatalf("retrieved body = %v, want 'Hello from stunt!'", retrieved["body"])
	}

	// ===== GET nonexistent message → 404 =====

	_, status = twilioGet(t, base+"/2010-06-01/Accounts/"+accountSID+"/Messages/SMnotfound.json")
	if status != 404 {
		t.Fatalf("GET nonexistent message -> status %d, want 404", status)
	}

	// ===== Async lifecycle: derive-on-read status machine =====
	// queued -> sent -> delivered (timings 1s/3s); a simulate_fail send
	// terminates in undelivered.

	body, status = twilioPostJSON(t, msgPath, map[string]any{
		"To":            "+15551234567",
		"From":          "+15557654321",
		"Body":          "doomed to bounce",
		"simulate_fail": true,
	})
	if status != 201 {
		t.Fatalf("POST fail message -> status %d, want 201; body %s", status, body)
	}
	var failMsg map[string]any
	if err := json.Unmarshal([]byte(body), &failMsg); err != nil {
		t.Fatalf("unmarshal fail message: %v (body %s)", err, body)
	}
	failSID, ok := failMsg["sid"].(string)
	if !ok || !strings.HasPrefix(failSID, "SM") {
		t.Fatalf("fail message sid = %v, want SM* prefix", failMsg["sid"])
	}

	time.Sleep(3500 * time.Millisecond)

	// Normal message -> delivered, with date_sent now set.
	body, status = twilioGet(t, base+"/2010-06-01/Accounts/"+accountSID+"/Messages/"+msgSID+".json")
	if status != 200 {
		t.Fatalf("GET delivered message -> status %d, want 200; body %s", status, body)
	}
	var delivered map[string]any
	if err := json.Unmarshal([]byte(body), &delivered); err != nil {
		t.Fatalf("unmarshal delivered: %v (body %s)", err, body)
	}
	if delivered["status"] != "delivered" {
		t.Fatalf("delivered status = %v, want delivered", delivered["status"])
	}
	if delivered["date_sent"] == nil {
		t.Fatalf("delivered date_sent = %v, want a timestamp", delivered["date_sent"])
	}

	// Failure-injected message -> undelivered with an error code.
	body, status = twilioGet(t, base+"/2010-06-01/Accounts/"+accountSID+"/Messages/"+failSID+".json")
	if status != 200 {
		t.Fatalf("GET undelivered message -> status %d, want 200; body %s", status, body)
	}
	var undelivered map[string]any
	if err := json.Unmarshal([]byte(body), &undelivered); err != nil {
		t.Fatalf("unmarshal undelivered: %v (body %s)", err, body)
	}
	if undelivered["status"] != "undelivered" {
		t.Fatalf("fail status = %v, want undelivered", undelivered["status"])
	}
	if undelivered["error_code"] == nil {
		t.Fatalf("fail error_code = %v, want a code", undelivered["error_code"])
	}

	// ===== POST call → 201, sid CA..., status queued =====

	callPath := base + "/2010-06-01/Accounts/" + accountSID + "/Calls.json"
	body, status = twilioPostJSON(t, callPath, map[string]any{
		"To":   "+15551234567",
		"From": "+15557654321",
		"Url":  "http://demo.twilio.com/docs/voice.xml",
	})
	if status != 201 {
		t.Fatalf("POST call -> status %d, want 201; body %s", status, body)
	}
	var call map[string]any
	if err := json.Unmarshal([]byte(body), &call); err != nil {
		t.Fatalf("unmarshal call: %v (body %s)", err, body)
	}
	callSID, ok := call["sid"].(string)
	if !ok || !strings.HasPrefix(callSID, "CA") {
		t.Fatalf("call sid = %v, want CA* prefix", call["sid"])
	}
	if call["status"] != "queued" {
		t.Fatalf("call status = %v, want queued", call["status"])
	}
	if call["direction"] != "outbound-api" {
		t.Fatalf("call direction = %v, want outbound-api", call["direction"])
	}

	// ===== Verify: create → pending =====

	verifyPath := base + "/v2/Services/VA00000000000000000000000000000000/Verification"
	body, status = twilioPostJSON(t, verifyPath, map[string]any{
		"To":      "+15555123456",
		"Channel": "sms",
	})
	if status != 201 {
		t.Fatalf("POST verification -> status %d, want 201; body %s", status, body)
	}
	var verif map[string]any
	if err := json.Unmarshal([]byte(body), &verif); err != nil {
		t.Fatalf("unmarshal verification: %v (body %s)", err, body)
	}
	if verif["status"] != "pending" {
		t.Fatalf("verification status = %v, want pending", verif["status"])
	}
	verifSID, ok := verif["sid"].(string)
	if !ok || !strings.HasPrefix(verifSID, "VL") {
		t.Fatalf("verification sid = %v, want VL* prefix", verif["sid"])
	}
	// Verify must NOT include the code in the response.
	if _, exists := verif["code"]; exists {
		t.Fatalf("verification response should not include 'code' field")
	}

	// ===== Verify check: wrong code → pending =====

	// The expected code is the last 6 digits of +15555123456 = "123456".
	checkPath := base + "/v2/Services/VA00000000000000000000000000000000/VerificationCheck"
	body, status = twilioPostJSON(t, checkPath, map[string]any{
		"To":   "+15555123456",
		"Code": "000000",
	})
	if status != 200 {
		t.Fatalf("POST verification check (wrong code) -> status %d, want 200; body %s", status, body)
	}
	var checkWrong map[string]any
	if err := json.Unmarshal([]byte(body), &checkWrong); err != nil {
		t.Fatalf("unmarshal check (wrong): %v (body %s)", err, body)
	}
	if checkWrong["status"] != "pending" {
		t.Fatalf("wrong code check status = %v, want pending", checkWrong["status"])
	}
	if checkWrong["valid"] != false {
		t.Fatalf("wrong code valid = %v, want false", checkWrong["valid"])
	}

	// ===== Verify check: correct code → approved =====

	body, status = twilioPostJSON(t, checkPath, map[string]any{
		"To":   "+15555123456",
		"Code": "123456",
	})
	if status != 200 {
		t.Fatalf("POST verification check (correct code) -> status %d, want 200; body %s", status, body)
	}
	var checkRight map[string]any
	if err := json.Unmarshal([]byte(body), &checkRight); err != nil {
		t.Fatalf("unmarshal check (correct): %v (body %s)", err, body)
	}
	if checkRight["status"] != "approved" {
		t.Fatalf("correct code check status = %v, want approved", checkRight["status"])
	}
	if checkRight["valid"] != true {
		t.Fatalf("correct code valid = %v, want true", checkRight["valid"])
	}

	// ===== Verify check: already approved → still approved (idempotent) =====

	body, status = twilioPostJSON(t, checkPath, map[string]any{
		"To":   "+15555123456",
		"Code": "123456",
	})
	if status != 200 {
		t.Fatalf("POST verification check (re-check) -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &checkRight); err != nil {
		t.Fatalf("unmarshal re-check: %v", err)
	}
	// After approval, re-checking with correct code should still show approved.
	if checkRight["status"] != "approved" {
		t.Fatalf("re-check status = %v, want approved", checkRight["status"])
	}

	// ===== 401 without auth =====

	_, status = twilioGetNoAuth(t, msgPath)
	if status != 401 {
		t.Fatalf("GET messages without auth -> status %d, want 401", status)
	}

	// ===== 401 with wrong credentials =====

	wrongAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("ACwrong:wrong"))
	req, _ := http.NewRequest("GET", msgPath, nil)
	req.Header.Set("Authorization", wrongAuth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("GET messages with wrong auth -> status %d, want 401", resp.StatusCode)
	}
}

// TestTwilioStyleMessageFilters pins the real MessageList filters: To, From,
// and the DateSent window params excluding unsent (queued) messages.
func TestTwilioStyleMessageFilters(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "twilio-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"twilio": {Adapter: absAdapterDir},
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

	base := addrs["twilio"]
	const accountSID = twilioAccountSID
	msgPath := base + "/2010-06-01/Accounts/" + accountSID + "/Messages.json"

	for _, to := range []string{"+15551110001", "+15551110001", "+15551110002"} {
		body, status := twilioPostJSON(t, msgPath, map[string]any{
			"To":   to,
			"From": "+15557654321",
			"Body": "filter me",
		})
		if status != 201 {
			t.Fatalf("POST message -> %d; %s", status, body)
		}
	}

	list := func(q string) []any {
		t.Helper()
		body, status := twilioGet(t, msgPath+q)
		if status != 200 {
			t.Fatalf("list %q -> %d; %s", q, status, body)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out["messages"].([]any)
	}

	if got := len(list("")); got != 3 {
		t.Fatalf("unfiltered -> %d messages, want 3", got)
	}
	if got := len(list("?To=" + url.QueryEscape("+15551110001"))); got != 2 {
		t.Fatalf("To filter -> %d messages, want 2", got)
	}
	if got := len(list("?To=" + url.QueryEscape("+15551110002"))); got != 1 {
		t.Fatalf("To filter -> %d messages, want 1", got)
	}
	if got := len(list("?From=" + url.QueryEscape("+15559999999"))); got != 0 {
		t.Fatalf("From miss -> %d messages, want 0", got)
	}
	// Queued messages have date_sent=null: a DateSent filter must exclude them.
	if got := len(list("?DateSent=2024-08-15")); got != 0 {
		t.Fatalf("DateSent on queued-only -> %d messages, want 0", got)
	}
	if got := len(list("?DateSent>=2020-01-01")); got != 0 {
		t.Fatalf("DateSent> on queued-only -> %d messages, want 0", got)
	}
}

// TestTwilioStyleLifecycleEmitsOnce pins the exactly-once guarantee: a
// terminal message read repeatedly (and listed again) must emit each
// lifecycle webhook once — the transition guard persists before emitting.
func TestTwilioStyleLifecycleEmitsOnce(t *testing.T) {
	var mu sync.Mutex
	var statuses []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev map[string]any
		if err := json.Unmarshal(b, &ev); err == nil {
			if ty, ok := ev["type"].(string); ok {
				mu.Lock()
				statuses = append(statuses, ty)
				mu.Unlock()
			}
		}
		w.WriteHeader(200)
	}))
	defer sink.Close()

	adapterDir := filepath.Join("..", "..", "adapters", "twilio-style")
	absAdapterDir, err := filepath.Abs(adapterDir)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"twilio": {Adapter: absAdapterDir, Config: map[string]any{"webhook_url": sink.URL}},
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

	base := addrs["twilio"]
	const accountSID = twilioAccountSID
	msgPath := base + "/2010-06-01/Accounts/" + accountSID + "/Messages.json"

	body, status := twilioPostJSON(t, msgPath, map[string]any{
		"To":   "+15551234567",
		"From": "+15557654321",
		"Body": "once only",
	})
	if status != 201 {
		t.Fatalf("POST message -> %d; %s", status, body)
	}
	var msg map[string]any
	_ = json.Unmarshal([]byte(body), &msg)
	sid := msg["sid"].(string)
	single := base + "/2010-06-01/Accounts/" + accountSID + "/Messages/" + sid + ".json"

	time.Sleep(3500 * time.Millisecond)

	// Repeated reads at terminal: single, list, single.
	for i := 0; i < 3; i++ {
		if _, st := twilioGet(t, single); st != 200 {
			t.Fatalf("read %d -> %d", i, st)
		}
		if _, st := twilioGet(t, msgPath); st != 200 {
			t.Fatalf("list %d -> %d", i, st)
		}
	}
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	counts := map[string]int{}
	for _, s := range statuses {
		counts[s]++
	}
	if counts["message.delivered"] != 1 {
		t.Errorf("delivered emitted %d times, want exactly 1 (statuses: %v)", counts["delivered"], counts)
	}
	if counts["message.sent"] != 1 {
		t.Errorf("sent emitted %d times, want exactly 1 (statuses: %v)", counts["sent"], counts)
	}
}
