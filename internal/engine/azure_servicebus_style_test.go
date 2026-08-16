package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestAzureServiceBusStyleAdapter exercises the azure-servicebus-style adapter:
//
//   - SAS token auth required: 401 without auth
//   - Service Bus send message → 201
//   - Service Bus receive message → 200 with body
//   - Receive again → 204 (empty)
//   - Topic info
//   - Storage queue send (JSON) → 201 XML response
//   - Storage queue receive → XML with MessageText

// sbSAS builds a real SharedAccessSignature token signed with the adapter's
// documented synthetic key (must match scripts/lib.star).
func sbSAS(resource string) string {
	secret := "stunt-servicebus-signing-key"
	se := "1900000000"
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(resource + "\n" + se))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return "SharedAccessSignature sr=" + url.QueryEscape(resource) + "&sig=" + url.QueryEscape(sig) + "&se=" + se + "&skn=RootManageSharedAccessKey"
}

func TestAzureServiceBusStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "azure-servicebus-style")
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
			"sb": {Adapter: absAdapterDir},
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

	base := addrs["sb"]
	sasToken := sbSAS("https://mybus.servicebus.windows.net/myqueue")

	// ===== 401 without auth =====

	_, status := sbPostJSON(t, base+"/myqueue/messages", "", map[string]any{"Body": "hello"})
	if status != 401 {
		t.Fatalf("send without auth -> status %d, want 401", status)
	}

	// ===== Service Bus send → 201 =====

	body, status := sbPostJSON(t, base+"/myqueue/messages", sasToken, map[string]any{
		"Body":        "Hello Service Bus!",
		"ContentType": "text/plain",
	})
	if status != 201 {
		t.Fatalf("send -> status %d, want 201; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	if _, ok := resp["MessageId"].(string); !ok {
		t.Fatalf("MessageId = %v, want string", resp["MessageId"])
	}
	if _, ok := resp["LockToken"].(string); !ok {
		t.Fatalf("LockToken = %v, want string", resp["LockToken"])
	}

	// ===== Service Bus receive → 200 with the sent body =====

	body, status = sbDelete(t, base+"/myqueue/messages/head", sasToken)
	if status != 200 {
		t.Fatalf("receive -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal receive: %v (body %s)", err, body)
	}
	if resp["Body"] != "Hello Service Bus!" {
		t.Fatalf("received Body = %v, want 'Hello Service Bus!'", resp["Body"])
	}
	if resp["ContentType"] != "text/plain" {
		t.Fatalf("received ContentType = %v, want 'text/plain'", resp["ContentType"])
	}

	// ===== Receive again → 204 (empty) =====

	body, status = sbDelete(t, base+"/myqueue/messages/head", sasToken)
	if status != 204 {
		t.Fatalf("receive (empty) -> status %d, want 204; body %s", status, body)
	}

	// ===== Topic info =====

	body, status = sbGet(t, base+"/$topicInfo?api-version=2024-01-01", sasToken)
	if status != 200 {
		t.Fatalf("topic info -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal topic: %v (body %s)", err, body)
	}
	if _, ok := resp["properties"].(map[string]any); !ok {
		t.Fatalf("properties = %v, want object", resp["properties"])
	}

	// ===== Storage queue send (JSON) → 201 XML response =====

	resp2 := sbPostRaw(t, base+"/mystorage/myqueue/messages", sasToken,
		`{"MessageText":"Storage message"}`, "application/json")
	defer resp2.Body.Close()
	b, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 201 {
		t.Fatalf("storage send -> status %d, want 201; body %s", resp2.StatusCode, string(b))
	}
	if !strings.Contains(string(b), "<MessageId>") {
		t.Fatalf("storage send response should contain XML, got: %s", string(b))
	}

	// ===== Storage queue receive → XML with MessageText =====

	resp3 := sbGetRaw(t, base+"/mystorage/myqueue/messages", sasToken)
	defer resp3.Body.Close()
	b, _ = io.ReadAll(resp3.Body)
	if resp3.StatusCode != 200 {
		t.Fatalf("storage receive -> status %d, want 200; body %s", resp3.StatusCode, string(b))
	}
	if !strings.Contains(string(b), "Storage message") {
		t.Fatalf("storage receive should contain MessageText, got: %s", string(b))
	}
	if !strings.Contains(string(b), "<QueueMessagesList>") {
		t.Fatalf("storage receive should contain <QueueMessagesList>, got: %s", string(b))
	}

	// ===== Bearer also works =====

	_, status = sbPostJSON(t, base+"/myqueue/messages", "Bearer testtoken", map[string]any{"Body": "bearer test"})
	if status != 201 {
		t.Fatalf("send with bearer -> status %d, want 201", status)
	}
}

// TestAzureServiceBusStylePeekLockAndTopics exercises the peek-lock receive
// cycle (receive/complete/renew/abandon/defer + 410 lock-lost), the
// topic/subscription surface (management CRUD + fan-out + subscription
// receive), and the Storage Queue visibility-timeout model:
//
//   - peek-lock receive → 200 with LockToken + LockedUntilUtc
//   - complete settles the message (next receive → 204)
//   - abandon releases the message (redelivered with DeliveryCount 2)
//   - defers it (normal receive → 204; ?sequencenumber receive → 200)
//   - renew extends the lock
//   - expired lock → 410 LockLost, and the message is returned to the queue
//   - unknown lock token / unknown action → 404
//   - topic + subscription CRUD (PUT create 201 / update 200, GET, DELETE)
//   - send-to-topic fans out a copy per subscription
//   - subscription peek-lock receive + settlement via the topic-scoped route
//   - storage receive hides for visibilitytimeout, then reappears (DequeueCount 2)
//   - pop-receipt delete → 204; missing receipt → 400; wrong receipt → 404
func TestAzureServiceBusStylePeekLockAndTopics(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "azure-servicebus-style")
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
			"sb": {Adapter: absAdapterDir},
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

	base := addrs["sb"]
	sasToken := sbSAS("https://mybus.servicebus.windows.net/myqueue")

	// ===== Peek-lock receive + complete =====

	sbPostJSON(t, base+"/plq/messages", sasToken, map[string]any{"Body": "locked work"})

	body, status := sbPostEmpty(t, base+"/plq/messages?receive=lock", sasToken)
	if status != 200 {
		t.Fatalf("peek-lock receive -> status %d, want 200; body %s", status, body)
	}
	var recv map[string]any
	if err := json.Unmarshal([]byte(body), &recv); err != nil {
		t.Fatalf("unmarshal peek-lock: %v (body %s)", err, body)
	}
	lockToken, ok := recv["LockToken"].(string)
	if !ok || lockToken == "" {
		t.Fatalf("LockToken = %v, want non-empty", recv["LockToken"])
	}
	if recv["Body"] != "locked work" {
		t.Fatalf("peek-lock Body = %v, want 'locked work'", recv["Body"])
	}
	lockedUntil, ok := recv["LockedUntilUtc"].(string)
	if !ok || lockedUntil == "" {
		t.Fatalf("LockedUntilUtc = %v, want non-empty", recv["LockedUntilUtc"])
	}
	if n, ok := recv["DeliveryCount"].(float64); !ok || n != 1 {
		t.Fatalf("DeliveryCount = %v, want 1", recv["DeliveryCount"])
	}

	// While locked, a second receiver sees nothing.
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock", sasToken)
	if status != 204 {
		t.Fatalf("peek-lock receive (locked) -> status %d, want 204; body %s", status, body)
	}

	body, status = sbPostEmpty(t, base+"/plq/messages/"+lockToken+"/complete", sasToken)
	if status != 200 {
		t.Fatalf("complete -> status %d, want 200; body %s", status, body)
	}

	// Completed message is gone.
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock", sasToken)
	if status != 204 {
		t.Fatalf("receive after complete -> status %d, want 204; body %s", status, body)
	}

	// ===== Abandon: lock released, message redelivered =====

	sbPostJSON(t, base+"/plq/messages", sasToken, map[string]any{"Body": "abandon me"})
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock", sasToken)
	if status != 200 {
		t.Fatalf("receive before abandon -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &recv); err != nil {
		t.Fatalf("unmarshal abandon receive: %v (body %s)", err, body)
	}
	abandonToken := recv["LockToken"].(string)
	body, status = sbPostEmpty(t, base+"/plq/messages/"+abandonToken+"/abandon", sasToken)
	if status != 200 {
		t.Fatalf("abandon -> status %d, want 200; body %s", status, body)
	}
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock", sasToken)
	if status != 200 {
		t.Fatalf("receive after abandon -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &recv); err != nil {
		t.Fatalf("unmarshal redelivery: %v (body %s)", err, body)
	}
	if n, ok := recv["DeliveryCount"].(float64); !ok || n != 2 {
		t.Fatalf("redelivery DeliveryCount = %v, want 2", recv["DeliveryCount"])
	}
	// Clean up: settle the redelivered message.
	_, status = sbPostEmpty(t, base+"/plq/messages/"+recv["LockToken"].(string)+"/complete", sasToken)
	if status != 200 {
		t.Fatalf("cleanup complete -> status %d, want 200", status)
	}

	// ===== Defer: invisible to normal receive, fetchable by sequence number =====

	sbPostJSON(t, base+"/plq/messages", sasToken, map[string]any{"Body": "defer me"})
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock", sasToken)
	if status != 200 {
		t.Fatalf("receive before defer -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &recv); err != nil {
		t.Fatalf("unmarshal defer receive: %v (body %s)", err, body)
	}
	seqNum := int(recv["SequenceNumber"].(float64))
	body, status = sbPostEmpty(t, base+"/plq/messages/"+recv["LockToken"].(string)+"/defer", sasToken)
	if status != 200 {
		t.Fatalf("defer -> status %d, want 200; body %s", status, body)
	}

	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock", sasToken)
	if status != 204 {
		t.Fatalf("receive after defer -> status %d, want 204; body %s", status, body)
	}
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock&sequencenumber="+strconv.Itoa(seqNum), sasToken)
	if status != 200 {
		t.Fatalf("sequencenumber receive -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &recv); err != nil {
		t.Fatalf("unmarshal sequencenumber receive: %v (body %s)", err, body)
	}
	_, status = sbPostEmpty(t, base+"/plq/messages/"+recv["LockToken"].(string)+"/complete", sasToken)
	if status != 200 {
		t.Fatalf("complete deferred -> status %d, want 200", status)
	}

	// ===== Renew extends the lock =====

	sbPostJSON(t, base+"/plq/messages", sasToken, map[string]any{"Body": "renew me"})
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock&lockduration=120", sasToken)
	if status != 200 {
		t.Fatalf("receive before renew -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &recv); err != nil {
		t.Fatalf("unmarshal renew receive: %v", err)
	}
	renewToken := recv["LockToken"].(string)
	body, status = sbPostEmpty(t, base+"/plq/messages/"+renewToken+"/renew?lockduration=300", sasToken)
	if status != 200 {
		t.Fatalf("renew -> status %d, want 200; body %s", status, body)
	}
	var renewResp map[string]any
	if err := json.Unmarshal([]byte(body), &renewResp); err != nil {
		t.Fatalf("unmarshal renew: %v (body %s)", err, body)
	}
	if renewResp["LockedUntilUtc"] == nil || renewResp["LockedUntilUtc"] == "" {
		t.Fatalf("renew LockedUntilUtc = %v, want non-empty", renewResp["LockedUntilUtc"])
	}
	_, status = sbPostEmpty(t, base+"/plq/messages/"+renewToken+"/complete", sasToken)
	if status != 200 {
		t.Fatalf("complete after renew -> status %d, want 200", status)
	}

	// ===== Expired lock → 410 LockLost; message returned to the queue =====

	sbPostJSON(t, base+"/plq/messages", sasToken, map[string]any{"Body": "short lock"})
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock&lockduration=1", sasToken)
	if status != 200 {
		t.Fatalf("receive short lock -> status %d, want 200", status)
	}
	if err := json.Unmarshal([]byte(body), &recv); err != nil {
		t.Fatalf("unmarshal short lock: %v", err)
	}
	shortToken := recv["LockToken"].(string)

	time.Sleep(1600 * time.Millisecond)

	body, status = sbPostEmpty(t, base+"/plq/messages/"+shortToken+"/complete", sasToken)
	if status != 410 {
		t.Fatalf("complete expired lock -> status %d, want 410; body %s", status, body)
	}
	if !strings.Contains(body, "LockLost") {
		t.Fatalf("expired lock body should contain LockLost, got %s", body)
	}
	body, status = sbPostEmpty(t, base+"/plq/messages/"+shortToken+"/renew", sasToken)
	if status != 410 {
		t.Fatalf("renew expired lock -> status %d, want 410", status)
	}
	// The lock-lost message is back on the queue.
	body, status = sbPostEmpty(t, base+"/plq/messages?receive=lock", sasToken)
	if status != 200 {
		t.Fatalf("receive after lock lost -> status %d, want 200; body %s", status, body)
	}

	// ===== Unknown lock token / unknown action → 404 =====

	body, status = sbPostEmpty(t, base+"/plq/messages/lock-token-999999/complete", sasToken)
	if status != 404 {
		t.Fatalf("unknown lock token -> status %d, want 404; body %s", status, body)
	}
	body, status = sbPostEmpty(t, base+"/plq/messages/"+shortToken+"/explode", sasToken)
	if status != 404 {
		t.Fatalf("unknown action -> status %d, want 404; body %s", status, body)
	}

	// ===== Topic + subscription management CRUD =====

	body, status = sbPutJSON(t, base+"/topics/orders", sasToken, map[string]any{
		"properties": map[string]any{"lockDuration": "PT60S"},
	})
	if status != 201 {
		t.Fatalf("create topic -> status %d, want 201; body %s", status, body)
	}
	var topicResp map[string]any
	if err := json.Unmarshal([]byte(body), &topicResp); err != nil {
		t.Fatalf("unmarshal create topic: %v (body %s)", err, body)
	}
	props, ok := topicResp["properties"].(map[string]any)
	if !ok || props["lockDuration"] != "PT60S" {
		t.Fatalf("topic properties = %v, want lockDuration PT60S", topicResp["properties"])
	}

	body, status = sbPutJSON(t, base+"/topics/orders", sasToken, map[string]any{
		"properties": map[string]any{"lockDuration": "PT90S"},
	})
	if status != 200 {
		t.Fatalf("update topic -> status %d, want 200; body %s", status, body)
	}

	body, status = sbGet(t, base+"/topics", sasToken)
	if status != 200 || !strings.Contains(body, "\"orders\"") {
		t.Fatalf("list topics -> status %d, want 200 with 'orders'; body %s", status, body)
	}
	body, status = sbGet(t, base+"/topics/orders", sasToken)
	if status != 200 {
		t.Fatalf("get topic -> status %d, want 200; body %s", status, body)
	}
	body, status = sbGet(t, base+"/topics/missing-topic", sasToken)
	if status != 404 {
		t.Fatalf("get missing topic -> status %d, want 404", status)
	}

	// Send to a topic with no subscriptions: message discarded, still 201.
	body, status = sbPostJSON(t, base+"/topics/orders/messages", sasToken, map[string]any{"Body": "no subs"})
	if status != 201 {
		t.Fatalf("send to topic (no subs) -> status %d, want 201; body %s", status, body)
	}
	var sendResp map[string]any
	if err := json.Unmarshal([]byte(body), &sendResp); err != nil {
		t.Fatalf("unmarshal topic send: %v", err)
	}
	if n, ok := sendResp["DeliveredSubscriptionCount"].(float64); !ok || n != 0 {
		t.Fatalf("DeliveredSubscriptionCount = %v, want 0", sendResp["DeliveredSubscriptionCount"])
	}

	// Subscriptions.
	body, status = sbPutJSON(t, base+"/topics/orders/subscriptions/audit", sasToken, map[string]any{})
	if status != 201 {
		t.Fatalf("create sub audit -> status %d, want 201; body %s", status, body)
	}
	body, status = sbPutJSON(t, base+"/topics/orders/subscriptions/billing", sasToken, map[string]any{})
	if status != 201 {
		t.Fatalf("create sub billing -> status %d, want 201; body %s", status, body)
	}
	body, status = sbPutJSON(t, base+"/topics/orders/subscriptions/audit", sasToken, map[string]any{})
	if status != 200 {
		t.Fatalf("update sub audit -> status %d, want 200; body %s", status, body)
	}
	body, status = sbGet(t, base+"/topics/orders/subscriptions", sasToken)
	if status != 200 || !strings.Contains(body, "audit") || !strings.Contains(body, "billing") {
		t.Fatalf("list subs -> status %d, want 200 with audit+billing; body %s", status, body)
	}
	body, status = sbGet(t, base+"/topics/orders/subscriptions/audit", sasToken)
	if status != 200 {
		t.Fatalf("get sub -> status %d, want 200; body %s", status, body)
	}
	body, status = sbPutJSON(t, base+"/topics/missing-topic/subscriptions/s1", sasToken, map[string]any{})
	if status != 404 {
		t.Fatalf("create sub under missing topic -> status %d, want 404; body %s", status, body)
	}

	// ===== Fan-out: both subscriptions get their own copy =====

	body, status = sbPostJSON(t, base+"/topics/orders/messages", sasToken, map[string]any{"Body": "fan out"})
	if status != 201 {
		t.Fatalf("send to topic -> status %d, want 201; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &sendResp); err != nil {
		t.Fatalf("unmarshal fanout send: %v", err)
	}
	if n, ok := sendResp["DeliveredSubscriptionCount"].(float64); !ok || n != 2 {
		t.Fatalf("DeliveredSubscriptionCount = %v, want 2", sendResp["DeliveredSubscriptionCount"])
	}

	// Peek-lock receive from the audit subscription.
	body, status = sbPostEmpty(t, base+"/topics/orders/subscriptions/audit/messages?receive=lock", sasToken)
	if status != 200 {
		t.Fatalf("sub receive -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &recv); err != nil {
		t.Fatalf("unmarshal sub receive: %v (body %s)", err, body)
	}
	if recv["Body"] != "fan out" {
		t.Fatalf("sub receive Body = %v, want 'fan out'", recv["Body"])
	}
	subToken := recv["LockToken"].(string)

	// Billing still holds its copy.
	body, status = sbPostEmpty(t, base+"/topics/orders/subscriptions/billing/messages?receive=lock", sasToken)
	if status != 200 {
		t.Fatalf("billing receive -> status %d, want 200; body %s", status, body)
	}
	var billRecv map[string]any
	if err := json.Unmarshal([]byte(body), &billRecv); err != nil {
		t.Fatalf("unmarshal billing receive: %v", err)
	}

	// Settle via the topic-scoped settlement route.
	body, status = sbPostEmpty(t, base+"/topics/orders/subscriptions/audit/messages/"+subToken+"/complete", sasToken)
	if status != 200 {
		t.Fatalf("sub complete -> status %d, want 200; body %s", status, body)
	}
	body, status = sbPostEmpty(t, base+"/topics/orders/subscriptions/audit/messages?receive=lock", sasToken)
	if status != 204 {
		t.Fatalf("sub receive after complete -> status %d, want 204; body %s", status, body)
	}

	// ===== Storage queue visibility timeout model =====

	sbPostRaw(t, base+"/storacct/visqueue/messages", sasToken, `{"MessageText":"vis msg"}`, "application/json")

	resp := sbGetRaw(t, base+"/storacct/visqueue/messages?visibilitytimeout=1", sasToken)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("storage receive -> status %d, want 200; body %s", resp.StatusCode, string(b))
	}
	if !strings.Contains(string(b), "<DequeueCount>1</DequeueCount>") {
		t.Fatalf("storage receive should show DequeueCount 1, got: %s", string(b))
	}
	pop := extractTag(t, string(b), "PopReceipt")
	if pop == "" {
		t.Fatalf("storage receive should include PopReceipt, got: %s", string(b))
	}

	// Hidden during the visibility window.
	resp = sbGetRaw(t, base+"/storacct/visqueue/messages?visibilitytimeout=1", sasToken)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "<QueueMessage>") {
		t.Fatalf("message should be invisible during visibility timeout, got: %s", string(b))
	}

	time.Sleep(1600 * time.Millisecond)

	// After the timeout it reappears with DequeueCount 2 and a new receipt.
	resp = sbGetRaw(t, base+"/storacct/visqueue/messages?visibilitytimeout=60", sasToken)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "<DequeueCount>2</DequeueCount>") {
		t.Fatalf("re-appeared message should show DequeueCount 2, got: %s", string(b))
	}
	pop2 := extractTag(t, string(b), "PopReceipt")
	if pop2 == "" || pop2 == pop {
		t.Fatalf("re-appeared message should have a fresh PopReceipt, old=%s new=%s", pop, pop2)
	}
	msgID := extractTag(t, string(b), "MessageId")

	// Pop-receipt delete while valid → 204.
	body, status = sbDelete(t, base+"/storacct/visqueue/messages/"+msgID+"?popreceipt="+pop2, sasToken)
	if status != 204 {
		t.Fatalf("storage delete -> status %d, want 204; body %s", status, body)
	}
	// Gone.
	resp = sbGetRaw(t, base+"/storacct/visqueue/messages", sasToken)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "<QueueMessage>") {
		t.Fatalf("deleted message should not come back, got: %s", string(b))
	}

	// ===== Storage delete failure paths =====

	sbPostRaw(t, base+"/storacct/visqueue/messages", sasToken, `{"MessageText":"del paths"}`, "application/json")
	resp = sbGetRaw(t, base+"/storacct/visqueue/messages?visibilitytimeout=60", sasToken)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	pop3 := extractTag(t, string(b), "PopReceipt")
	msgID3 := extractTag(t, string(b), "MessageId")

	body, status = sbDelete(t, base+"/storacct/visqueue/messages/"+msgID3, sasToken)
	if status != 400 {
		t.Fatalf("storage delete without popreceipt -> status %d, want 400; body %s", status, body)
	}
	body, status = sbDelete(t, base+"/storacct/visqueue/messages/"+msgID3+"?popreceipt=wrong-receipt", sasToken)
	if status != 404 {
		t.Fatalf("storage delete with wrong popreceipt -> status %d, want 404; body %s", status, body)
	}
	body, status = sbDelete(t, base+"/storacct/visqueue/messages/storage-msg-999999?popreceipt="+pop3, sasToken)
	if status != 404 {
		t.Fatalf("storage delete unknown message -> status %d, want 404; body %s", status, body)
	}

	// ===== Subscription + topic delete =====

	body, status = sbDelete(t, base+"/topics/orders/subscriptions/billing", sasToken)
	if status != 200 {
		t.Fatalf("delete sub -> status %d, want 200; body %s", status, body)
	}
	body, status = sbGet(t, base+"/topics/orders/subscriptions/billing", sasToken)
	if status != 404 {
		t.Fatalf("get deleted sub -> status %d, want 404", status)
	}
	body, status = sbDelete(t, base+"/topics/orders", sasToken)
	if status != 200 {
		t.Fatalf("delete topic -> status %d, want 200; body %s", status, body)
	}
	body, status = sbGet(t, base+"/topics/orders", sasToken)
	if status != 404 {
		t.Fatalf("get deleted topic -> status %d, want 404", status)
	}
	// Sending to the deleted topic fails.
	body, status = sbPostJSON(t, base+"/topics/orders/messages", sasToken, map[string]any{"Body": "gone"})
	if status != 404 {
		t.Fatalf("send to deleted topic -> status %d, want 404; body %s", status, body)
	}
}

// extractTag pulls the first <tag>...</tag> value out of an XML string.
func extractTag(t *testing.T, xmlBody, tag string) string {
	t.Helper()
	re := regexp.MustCompile(`<` + tag + `>(.*?)</` + tag + `>`)
	m := re.FindStringSubmatch(xmlBody)
	if m == nil {
		return ""
	}
	return m[1]
}

// sbPostEmpty issues a POST with no body.
func sbPostEmpty(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// sbPutJSON issues a PUT with a JSON body.
func sbPutJSON(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

// === Azure Service Bus test helpers ===

func sbPostJSON(t *testing.T, rawurl, auth string, body map[string]any) (string, int) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sbPostRaw(t *testing.T, rawurl, auth, body, contentType string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", rawurl, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func sbDelete(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sbGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func sbGetRaw(t *testing.T, rawurl, auth string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
