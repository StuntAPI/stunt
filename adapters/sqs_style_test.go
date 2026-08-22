package adapters

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// These tests drive the sqs-style adapter scripts directly (lib.star
// preloaded) over a shared store and a VIRTUAL clock, covering the
// time-dependent behaviors an HTTP-level test cannot reach without waiting:
// visibility-timeout expiry, per-receive overrides, ChangeMessageVisibility,
// send delays, and the purge window — plus real SigV4 signing (positive and
// rejection paths) with the documented synthetic credentials.

// Synthetic AWS credentials the adapter documents in its README.
const (
	sqsStyleAccessKey = "AKIAIOSFODNN7EXAMPLE"
	sqsStyleSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	sqsStyleBadSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYWRONGSECRET0"
	sqsStyleRegion    = "us-east-1"
)

// sqsFixture is one shared store + virtual clock with a loaded VM per
// handler script (service.star for POST /, queues.star for POST
// /{queueName}); both VMs observe the same collections/kv/blob state, like
// the engine.
type sqsFixture struct {
	t     *testing.T
	vc    *clock.Clock
	vmSvc *starlark.VM
	vmQ   *starlark.VM
	host  string
}

func newSQSFixture(t *testing.T, start time.Time) *sqsFixture {
	return newSQSFixtureProfile(t, start, nil)
}

// newSQSFixtureProfile is newSQSFixture with a fake active profile backing
// profile_active() (nil = no profile active).
func newSQSFixtureProfile(t *testing.T, start time.Time, active func() string) *sqsFixture {
	t.Helper()
	dir := repoAdaptersDir(t)
	root := filepath.Join(dir, "sqs-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}

	tmp := t.TempDir()
	store, err := primitives.Open(filepath.Join(tmp, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	kvStore, err := kv.Open(filepath.Join(tmp, "s.kv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kvStore.Close() })
	blobStore, err := blob.Open(filepath.Join(tmp, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store:         store,
		KV:            kvStore,
		Blob:          blobStore,
		Clock:         vc,
		ServiceName:   "test",
		ActiveProfile: active,
	})

	load := func(script string) *starlark.VM {
		t.Helper()
		src, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
		if err != nil {
			t.Fatalf("LoadWithLib %s: %v", script, err)
		}
		return vm
	}

	return &sqsFixture{
		t:     t,
		vc:    vc,
		vmSvc: load("service.star"),
		vmQ:   load("queues.star"),
		host:  "sqs.stunt.test",
	}
}

// --- SigV4 (mirrors the adapter's recomputation: service "sqs") ---

func sqsMD5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func sqsSHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sqsHMAC(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// sqsSigV4Auth returns an Authorization header value for a POST with the
// given path/body/host, signing host + x-amz-date (the shape SQS SDKs sign).
func sqsSigV4Auth(path string, body []byte, host string, at time.Time, accessKey, secretKey string) string {
	amzDate := at.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	payloadHash := sqsSHA256Hex(body)
	signedHeaders := "host;x-amz-date"
	creq := "POST\n" +
		path + "\n" +
		"" + "\n" + // no query string
		"host:" + host + "\n" +
		"x-amz-date:" + amzDate + "\n" +
		"\n" +
		signedHeaders + "\n" +
		payloadHash
	scope := date + "/" + sqsStyleRegion + "/sqs/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sqsSHA256Hex([]byte(creq))
	kDate := sqsHMAC([]byte("AWS4"+secretKey), []byte(date))
	kRegion := sqsHMAC(kDate, []byte(sqsStyleRegion))
	kService := sqsHMAC(kRegion, []byte("sqs"))
	key := sqsHMAC(kService, []byte("aws4_request"))
	sig := hex.EncodeToString(sqsHMAC(key, []byte(sts)))
	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + sig
}

// sqsTamperSignature flips the first hex digit of an already-built
// Authorization header, keeping it well-formed.
func sqsTamperSignature(auth string) string {
	i := strings.Index(auth, "Signature=")
	if i < 0 {
		return auth
	}
	pos := i + len("Signature=")
	flip := "0"
	if auth[pos] == '0' {
		flip = "1"
	}
	return auth[:pos] + flip + auth[pos+1:]
}

// --- call helpers ---

// svcCallAt invokes an operation over the endpoint transport (POST /, queue
// in the QueueUrl body field), SigV4-signed at the given time with the
// documented credentials.
func (f *sqsFixture) svcCallAt(op string, payload map[string]any, at time.Time, accessKey, secretKey string) starlark.Response {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	headers := map[string]string{
		"X-Amz-Target":  "AmazonSQS." + op,
		"X-Amz-Date":    at.UTC().Format("20060102T150405Z"),
		"Authorization": sqsSigV4Auth("/", raw, f.host, at, accessKey, secretKey),
	}
	resp, err := f.vmSvc.Call("on_service_api", starlark.Request{
		Method:  "POST",
		Path:    "/",
		Host:    f.host,
		Headers: headers,
		RawBody: string(raw),
	})
	if err != nil {
		f.t.Fatalf("on_service_api %s: %v", op, err)
	}
	return resp
}

func (f *sqsFixture) svcCall(op string, payload map[string]any) starlark.Response {
	return f.svcCallAt(op, payload, f.vc.Now(), sqsStyleAccessKey, sqsStyleSecretKey)
}

// qCall invokes an operation over the queue-URL transport (POST /{queueName},
// queue from the path param), SigV4-signed with the documented credentials.
func (f *sqsFixture) qCall(queue, op string, payload map[string]any) starlark.Response {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	at := f.vc.Now()
	headers := map[string]string{
		"X-Amz-Target":  "AmazonSQS." + op,
		"X-Amz-Date":    at.UTC().Format("20060102T150405Z"),
		"Authorization": sqsSigV4Auth("/"+queue, raw, f.host, at, sqsStyleAccessKey, sqsStyleSecretKey),
	}
	resp, err := f.vmQ.Call("on_queue_url_api", starlark.Request{
		Method:  "POST",
		Path:    "/" + queue,
		Host:    f.host,
		Headers: headers,
		RawBody: string(raw),
		Params:  map[string]string{"queueName": queue},
	})
	if err != nil {
		f.t.Fatalf("on_queue_url_api %s: %v", op, err)
	}
	return resp
}

// rawSvcCall invokes the endpoint transport with arbitrary headers (no
// signing) for the rejection paths.
func (f *sqsFixture) rawSvcCall(op string, payload map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, ok := headers["X-Amz-Target"]; !ok {
		headers["X-Amz-Target"] = "AmazonSQS." + op
	}
	resp, err := f.vmSvc.Call("on_service_api", starlark.Request{
		Method:  "POST",
		Path:    "/",
		Host:    f.host,
		Headers: headers,
		RawBody: string(raw),
	})
	if err != nil {
		f.t.Fatalf("on_service_api %s: %v", op, err)
	}
	return resp
}

// sqsErrType digs __type out of the SQS error envelope (prefix stripped).
func sqsErrType(t *testing.T, resp starlark.Response) string {
	t.Helper()
	et, _ := resp.Body["__type"].(string)
	return strings.TrimPrefix(et, "com.amazonaws.sqs#")
}

func sqsBodyStr(resp starlark.Response, key string) string {
	s, _ := resp.Body[key].(string)
	return s
}

// TestSQSSigV4Verification: a request signed with the documented synthetic
// credentials verifies (both transports); missing/garbage/tampered/wrong-key
// auth and a stale x-amz-date are rejected in the SQS error envelope.
func TestSQSSigV4Verification(t *testing.T) {
	base := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	f := newSQSFixture(t, base)

	// Positive path over POST /: CreateQueue returns the documented URL shape.
	resp := f.svcCall("CreateQueue", map[string]any{"QueueName": "sig-demo"})
	if resp.Status != 200 {
		t.Fatalf("CreateQueue -> %d: %v", resp.Status, resp.Body)
	}
	if got := sqsBodyStr(resp, "QueueUrl"); got != "http://"+f.host+"/sig-demo" {
		t.Fatalf("QueueUrl = %q, want http://%s/sig-demo", got, f.host)
	}

	// Positive path over the queue-URL transport: Send addressed by path.
	send := f.qCall("sig-demo", "SendMessage", map[string]any{
		"QueueUrl":    "http://" + f.host + "/sig-demo",
		"MessageBody": "sigv4-body",
	})
	if send.Status != 200 {
		t.Fatalf("queue-URL SendMessage -> %d: %v", send.Status, send.Body)
	}
	if got := sqsBodyStr(send, "MD5OfMessageBody"); got != sqsMD5Hex([]byte("sigv4-body")) {
		t.Fatalf("MD5OfMessageBody = %q, want the real MD5 of the body (%q)", got, sqsMD5Hex([]byte("sigv4-body")))
	}

	// No Authorization header -> 403 MissingAuthenticationToken.
	resp = f.rawSvcCall("ListQueues", map[string]any{}, map[string]string{})
	if resp.Status != 403 || sqsErrType(t, resp) != "MissingAuthenticationToken" {
		t.Fatalf("no auth -> %d %q, want 403 MissingAuthenticationToken", resp.Status, sqsErrType(t, resp))
	}

	// Garbage scheme -> 403 InvalidSignatureException.
	resp = f.rawSvcCall("ListQueues", map[string]any{}, map[string]string{"Authorization": "Bearer some-token"})
	if resp.Status != 403 || sqsErrType(t, resp) != "InvalidSignatureException" {
		t.Fatalf("garbage auth -> %d %q, want 403 InvalidSignatureException", resp.Status, sqsErrType(t, resp))
	}

	// Tampered signature (well-formed header, flipped hex digit) -> 403.
	raw, _ := json.Marshal(map[string]any{})
	at := f.vc.Now()
	tampered := map[string]string{
		"X-Amz-Date":    at.UTC().Format("20060102T150405Z"),
		"Authorization": sqsTamperSignature(sqsSigV4Auth("/", raw, f.host, at, sqsStyleAccessKey, sqsStyleSecretKey)),
	}
	resp = f.rawSvcCall("ListQueues", map[string]any{}, tampered)
	if resp.Status != 403 || sqsErrType(t, resp) != "InvalidSignatureException" {
		t.Fatalf("tampered signature -> %d %q, want 403 InvalidSignatureException", resp.Status, sqsErrType(t, resp))
	}

	// Wrong secret -> 403 InvalidSignatureException (recomputation mismatch).
	resp = f.svcCallAt("ListQueues", map[string]any{}, f.vc.Now(), sqsStyleAccessKey, sqsStyleBadSecret)
	if resp.Status != 403 || sqsErrType(t, resp) != "InvalidSignatureException" {
		t.Fatalf("wrong secret -> %d %q, want 403 InvalidSignatureException", resp.Status, sqsErrType(t, resp))
	}

	// Unknown access key -> 403 InvalidClientTokenId.
	resp = f.svcCallAt("ListQueues", map[string]any{}, f.vc.Now(), "AKIAUNKNOWNKEYMOCK1", sqsStyleSecretKey)
	if resp.Status != 403 || sqsErrType(t, resp) != "InvalidClientTokenId" {
		t.Fatalf("unknown AKID -> %d %q, want 403 InvalidClientTokenId", resp.Status, sqsErrType(t, resp))
	}

	// Signed at the fixture start, clock advanced past the skew window ->
	// 403 RequestTimeTooSkewed; a fresh signature at the new time works.
	f.vc.Advance(20 * time.Minute)
	resp = f.svcCallAt("ListQueues", map[string]any{}, base, sqsStyleAccessKey, sqsStyleSecretKey)
	if resp.Status != 403 || sqsErrType(t, resp) != "RequestTimeTooSkewed" {
		t.Fatalf("stale x-amz-date -> %d %q, want 403 RequestTimeTooSkewed", resp.Status, sqsErrType(t, resp))
	}
	resp = f.svcCall("ListQueues", map[string]any{})
	if resp.Status != 200 {
		t.Fatalf("fresh signed ListQueues -> %d: %v", resp.Status, resp.Body)
	}
}

// TestSQSVisibilityTimeoutLifecycle drives the whole visibility model with
// the virtual clock: receive hides the message until the timeout lapses
// (queue default, per-receive override), the next receive mints a fresh
// receipt handle and bumps ApproximateReceiveCount, ChangeMessageVisibility
// extends the window, DeleteMessage removes the message for good, and stale
// handles fail with ReceiptHandleIsInvalid.
func TestSQSVisibilityTimeoutLifecycle(t *testing.T) {
	start := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	f := newSQSFixture(t, start)

	if resp := f.svcCall("CreateQueue", map[string]any{"QueueName": "jobs"}); resp.Status != 200 {
		t.Fatalf("CreateQueue -> %d: %v", resp.Status, resp.Body)
	}

	// DelaySeconds on send: not receivable until the delay lapses.
	if resp := f.svcCall("SendMessage", map[string]any{
		"QueueUrl": "http://" + f.host + "/delayed", "MessageBody": "never",
	}); resp.Status != 400 || sqsErrType(t, resp) != "QueueDoesNotExist" {
		t.Fatalf("send to unknown queue -> %d %q, want 400 QueueDoesNotExist", resp.Status, sqsErrType(t, resp))
	}
	if resp := f.svcCall("SendMessage", map[string]any{
		"QueueUrl": "http://" + f.host + "/jobs", "MessageBody": "delayed-job", "DelaySeconds": 10,
	}); resp.Status != 200 {
		t.Fatalf("delayed send -> %d: %v", resp.Status, resp.Body)
	}
	if resp := f.svcCall("ReceiveMessage", map[string]any{
		"QueueUrl": "http://" + f.host + "/jobs",
	}); resp.Status != 200 {
		t.Fatalf("receive -> %d: %v", resp.Status, resp.Body)
	} else if _, present := resp.Body["Messages"]; present {
		t.Fatalf("delayed message was receivable before its delay lapsed: %v", resp.Body)
	}
	f.vc.Advance(10 * time.Second)

	// MaxNumberOfMessages bounds: 0 and 11 are rejected.
	for _, bad := range []int{0, 11} {
		resp := f.svcCall("ReceiveMessage", map[string]any{
			"QueueUrl":              "http://" + f.host + "/jobs",
			"MaxNumberOfMessages":   bad,
			"MessageAttributeNames": []string{"All"},
		})
		if resp.Status != 400 || sqsErrType(t, resp) != "InvalidParameterValue" {
			t.Fatalf("MaxNumberOfMessages %d -> %d %q, want 400 InvalidParameterValue", bad, resp.Status, sqsErrType(t, resp))
		}
	}

	// First receive: per-receive VisibilityTimeout override of 5s.
	first := f.svcCall("ReceiveMessage", map[string]any{
		"QueueUrl":            "http://" + f.host + "/jobs",
		"VisibilityTimeout":   5,
		"AttributeNames":      []string{"All"},
		"WaitTimeSeconds":     10, // accepted, never honored (no long-poll)
		"MaxNumberOfMessages": 1,
	})
	msgs, ok := first.Body["Messages"].([]any)
	if first.Status != 200 || !ok || len(msgs) != 1 {
		t.Fatalf("first receive -> %d: %v", first.Status, first.Body)
	}
	msg1 := msgs[0].(map[string]any)
	id1, _ := msg1["MessageId"].(string)
	handle1, _ := msg1["ReceiptHandle"].(string)
	if msg1["Body"] != "delayed-job" || id1 == "" || handle1 == "" {
		t.Fatalf("first receive message = %v", msg1)
	}
	if msg1["MD5OfBody"] != sqsMD5Hex([]byte("delayed-job")) {
		t.Fatalf("MD5OfBody = %v, want the real MD5 of the body", msg1["MD5OfBody"])
	}
	attrs1 := msg1["Attributes"].(map[string]any)
	if attrs1["ApproximateReceiveCount"] != "1" {
		t.Fatalf("ApproximateReceiveCount = %v, want 1", attrs1["ApproximateReceiveCount"])
	}
	// Message system attributes are epoch milliseconds on the SQS wire
	// (queue attributes stay seconds) — see _message_attribute_map.
	if attrs1["SentTimestamp"] != fmt.Sprintf("%d", start.Unix()*1000) {
		t.Fatalf("SentTimestamp = %v, want sent-at epoch ms %d", attrs1["SentTimestamp"], start.Unix()*1000)
	}
	if attrs1["ApproximateFirstReceiveTimestamp"] != fmt.Sprintf("%d", start.Add(10*time.Second).Unix()*1000) {
		t.Fatalf("ApproximateFirstReceiveTimestamp = %v, want first-receive epoch ms", attrs1["ApproximateFirstReceiveTimestamp"])
	}

	// While in flight (before the 5s override lapses) nothing is receivable
	// and the queue counts report one not-visible message.
	again := f.svcCall("ReceiveMessage", map[string]any{"QueueUrl": "http://" + f.host + "/jobs"})
	if _, present := again.Body["Messages"]; present {
		t.Fatalf("in-flight message was receivable: %v", again.Body)
	}
	counts := f.svcCall("GetQueueAttributes", map[string]any{
		"QueueUrl":       "http://" + f.host + "/jobs",
		"AttributeNames": []string{"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible", "VisibilityTimeout", "QueueArn"},
	})
	if counts.Status != 200 {
		t.Fatalf("GetQueueAttributes -> %d: %v", counts.Status, counts.Body)
	}
	ca := counts.Body["Attributes"].(map[string]any)
	if ca["ApproximateNumberOfMessages"] != "0" || ca["ApproximateNumberOfMessagesNotVisible"] != "1" {
		t.Fatalf("in-flight counts = %v, want 0 visible / 1 not visible", ca)
	}
	if ca["VisibilityTimeout"] != "30" {
		t.Fatalf("VisibilityTimeout = %v, want the 30s default", ca["VisibilityTimeout"])
	}

	// After the timeout lapses the message is redeliverable with a FRESH
	// receipt handle and ApproximateReceiveCount 2.
	f.vc.Advance(5 * time.Second)
	second := f.svcCall("ReceiveMessage", map[string]any{
		"QueueUrl":       "http://" + f.host + "/jobs",
		"AttributeNames": []string{"All"},
	})
	msgs, ok = second.Body["Messages"].([]any)
	if second.Status != 200 || !ok || len(msgs) != 1 {
		t.Fatalf("receive after timeout -> %d: %v", second.Status, second.Body)
	}
	msg2 := msgs[0].(map[string]any)
	handle2, _ := msg2["ReceiptHandle"].(string)
	if msg2["MessageId"] != id1 {
		t.Fatalf("redelivered MessageId = %v, want %v", msg2["MessageId"], id1)
	}
	if handle2 == handle1 {
		t.Fatal("redelivery returned the SAME receipt handle, want a fresh one per receive")
	}
	if msg2["Attributes"].(map[string]any)["ApproximateReceiveCount"] != "2" {
		t.Fatalf("ApproximateReceiveCount = %v, want 2", msg2["Attributes"])
	}

	// ChangeMessageVisibility extends the in-flight window: +100s keeps the
	// message hidden past the original timeout, then it lapses.
	extend := f.svcCall("ChangeMessageVisibility", map[string]any{
		"QueueUrl":          "http://" + f.host + "/jobs",
		"ReceiptHandle":     handle2,
		"VisibilityTimeout": 100,
	})
	if extend.Status != 200 {
		t.Fatalf("ChangeMessageVisibility -> %d: %v", extend.Status, extend.Body)
	}
	f.vc.Advance(6 * time.Second)
	if again := f.svcCall("ReceiveMessage", map[string]any{"QueueUrl": "http://" + f.host + "/jobs"}); again.Status != 200 {
		t.Fatalf("receive within extended window -> %d: %v", again.Status, again.Body)
	} else if _, present := again.Body["Messages"]; present {
		t.Fatalf("message visible inside the extended window: %v", again.Body)
	}
	f.vc.Advance(95 * time.Second)
	third := f.svcCall("ReceiveMessage", map[string]any{"QueueUrl": "http://" + f.host + "/jobs"})
	msgs, ok = third.Body["Messages"].([]any)
	if third.Status != 200 || !ok || len(msgs) != 1 {
		t.Fatalf("receive after extended window -> %d: %v", third.Status, third.Body)
	}
	handle3, _ := msgs[0].(map[string]any)["ReceiptHandle"].(string)

	// DeleteMessage removes the message for good.
	if resp := f.svcCall("DeleteMessage", map[string]any{
		"QueueUrl": "http://" + f.host + "/jobs", "ReceiptHandle": handle3,
	}); resp.Status != 200 {
		t.Fatalf("DeleteMessage -> %d: %v", resp.Status, resp.Body)
	}
	if resp := f.svcCall("ReceiveMessage", map[string]any{"QueueUrl": "http://" + f.host + "/jobs"}); resp.Status != 200 {
		t.Fatalf("receive after delete -> %d: %v", resp.Status, resp.Body)
	} else if _, present := resp.Body["Messages"]; present {
		t.Fatalf("deleted message was receivable: %v", resp.Body)
	}

	// Handles from earlier receives (and a re-used handle) are invalid.
	for _, stale := range []string{handle1, handle2, handle3} {
		resp := f.svcCall("DeleteMessage", map[string]any{
			"QueueUrl": "http://" + f.host + "/jobs", "ReceiptHandle": stale,
		})
		if resp.Status != 400 || sqsErrType(t, resp) != "ReceiptHandleIsInvalid" {
			t.Fatalf("stale handle delete -> %d %q, want 400 ReceiptHandleIsInvalid", resp.Status, sqsErrType(t, resp))
		}
	}
}

// TestSQSSendMessageBatchPartialFailure covers the batch shape: per-entry
// Id/MessageId echoes, partial failure (a bad entry fails alone, the rest
// still send), message-attribute pass-through on receive, and the batch-level
// guards (too many entries, duplicate ids).
func TestSQSSendMessageBatchPartialFailure(t *testing.T) {
	f := newSQSFixture(t, time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC))
	const queueURL = "http://sqs.stunt.test/batch-q"

	if resp := f.svcCall("CreateQueue", map[string]any{"QueueName": "batch-q"}); resp.Status != 200 {
		t.Fatalf("CreateQueue -> %d: %v", resp.Status, resp.Body)
	}

	entries := []map[string]any{
		{"Id": "id-1", "MessageBody": "first"},
		{"Id": "id-2", "MessageBody": ""}, // fails alone
		{"Id": "id-3", "MessageBody": "third", "MessageAttributes": map[string]any{
			"priority": map[string]any{"DataType": "Number", "StringValue": "1"},
		}},
	}
	resp := f.svcCall("SendMessageBatch", map[string]any{"QueueUrl": queueURL, "Entries": entries})
	if resp.Status != 200 {
		t.Fatalf("SendMessageBatch -> %d: %v", resp.Status, resp.Body)
	}
	successful, _ := resp.Body["Successful"].([]any)
	failed, _ := resp.Body["Failed"].([]any)
	if len(successful) != 2 || len(failed) != 1 {
		t.Fatalf("batch result = %d successful / %d failed, want 2/1 (body %v)", len(successful), len(failed), resp.Body)
	}
	ok1 := successful[0].(map[string]any)
	ok3 := successful[1].(map[string]any)
	if ok1["Id"] != "id-1" || ok3["Id"] != "id-3" {
		t.Fatalf("Successful ids = %v / %v, want id-1 / id-3", ok1["Id"], ok3["Id"])
	}
	if ok1["MessageId"] == "" || ok1["MessageId"] == ok3["MessageId"] {
		t.Fatalf("Successful MessageIds = %v / %v, want distinct non-empty", ok1["MessageId"], ok3["MessageId"])
	}
	if ok1["MD5OfMessageBody"] != sqsMD5Hex([]byte("first")) {
		t.Fatalf("MD5OfMessageBody = %v, want the real MD5 of the entry body", ok1["MD5OfMessageBody"])
	}
	fail := failed[0].(map[string]any)
	if fail["Id"] != "id-2" || fail["SenderFault"] != true || fail["Code"] != "InvalidParameterValue" {
		t.Fatalf("Failed entry = %v, want id-2 sender-fault InvalidParameterValue", fail)
	}

	// Both good entries are receivable; the failed one never landed. The
	// attribute-bearing entry round-trips its MessageAttributes.
	recv := f.svcCall("ReceiveMessage", map[string]any{
		"QueueUrl":              queueURL,
		"MaxNumberOfMessages":   10,
		"AttributeNames":        []string{"All"},
		"MessageAttributeNames": []string{"All"},
	})
	msgs, ok := recv.Body["Messages"].([]any)
	if recv.Status != 200 || !ok || len(msgs) != 2 {
		t.Fatalf("receive after batch -> %d: %v", recv.Status, recv.Body)
	}
	bodies := map[string]map[string]any{}
	for _, m := range msgs {
		m := m.(map[string]any)
		bodies[m["Body"].(string)] = m
	}
	if _, present := bodies["second"]; present {
		t.Fatal("failed batch entry was delivered")
	}
	third := bodies["third"]
	mattrs, ok := third["MessageAttributes"].(map[string]any)
	if !ok {
		t.Fatalf("MessageAttributes not returned: %v", third)
	}
	prio, ok := mattrs["priority"].(map[string]any)
	if !ok || prio["DataType"] != "Number" || prio["StringValue"] != "1" {
		t.Fatalf("message attribute did not round-trip: %v", mattrs)
	}
	if _, present := third["MD5OfMessageAttributes"]; present {
		t.Fatal("MD5OfMessageAttributes missing on the attributed message")
	}
	if _, present := bodies["first"]["MessageAttributes"]; present {
		t.Fatal("MessageAttributes returned for a message that sent none")
	}

	// Batch guards: more than the max entries, and duplicate entry ids.
	tooMany := []map[string]any{}
	for i := 0; i < 11; i++ {
		tooMany = append(tooMany, map[string]any{"Id": "e" + string(rune('a'+i)), "MessageBody": "x"})
	}
	resp = f.svcCall("SendMessageBatch", map[string]any{"QueueUrl": queueURL, "Entries": tooMany})
	if resp.Status != 400 || sqsErrType(t, resp) != "TooManyEntriesInBatchRequest" {
		t.Fatalf("oversize batch -> %d %q, want 400 TooManyEntriesInBatchRequest", resp.Status, sqsErrType(t, resp))
	}
	resp = f.svcCall("SendMessageBatch", map[string]any{"QueueUrl": queueURL, "Entries": []map[string]any{
		{"Id": "dup", "MessageBody": "a"}, {"Id": "dup", "MessageBody": "b"},
	}})
	if resp.Status != 400 || sqsErrType(t, resp) != "BatchEntryIdsNotDistinct" {
		t.Fatalf("duplicate ids -> %d %q, want 400 BatchEntryIdsNotDistinct", resp.Status, sqsErrType(t, resp))
	}
}

// TestSQSPurgeQueueWindow: purge empties the queue immediately (the
// documented divergence) but enforces the real one-purge-per-60s rule via the
// clock — an immediate second purge fails with 403 PurgeQueueInProgress.
func TestSQSPurgeQueueWindow(t *testing.T) {
	f := newSQSFixture(t, time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC))
	const queueURL = "http://sqs.stunt.test/purge-q"

	if resp := f.svcCall("CreateQueue", map[string]any{"QueueName": "purge-q"}); resp.Status != 200 {
		t.Fatalf("CreateQueue -> %d: %v", resp.Status, resp.Body)
	}
	for _, b := range []string{"p-1", "p-2"} {
		if resp := f.svcCall("SendMessage", map[string]any{"QueueUrl": queueURL, "MessageBody": b}); resp.Status != 200 {
			t.Fatalf("send %q -> %d: %v", b, resp.Status, resp.Body)
		}
	}

	// Purge over the queue-URL transport (path-addressed).
	if resp := f.qCall("purge-q", "PurgeQueue", map[string]any{"QueueUrl": queueURL}); resp.Status != 200 {
		t.Fatalf("PurgeQueue -> %d: %v", resp.Status, resp.Body)
	}
	if resp := f.svcCall("ReceiveMessage", map[string]any{"QueueUrl": queueURL, "MaxNumberOfMessages": 10}); resp.Status != 200 {
		t.Fatalf("receive after purge -> %d: %v", resp.Status, resp.Body)
	} else if _, present := resp.Body["Messages"]; present {
		t.Fatalf("messages survived the purge: %v", resp.Body)
	}

	// Immediate second purge inside the window -> 403 PurgeQueueInProgress.
	resp := f.svcCall("PurgeQueue", map[string]any{"QueueUrl": queueURL})
	if resp.Status != 403 || sqsErrType(t, resp) != "PurgeQueueInProgress" {
		t.Fatalf("second purge -> %d %q, want 403 PurgeQueueInProgress", resp.Status, sqsErrType(t, resp))
	}

	// Past the window, purging works again.
	f.vc.Advance(61 * time.Second)
	if resp := f.svcCall("PurgeQueue", map[string]any{"QueueUrl": queueURL}); resp.Status != 200 {
		t.Fatalf("purge after window -> %d: %v", resp.Status, resp.Body)
	}
}

// TestSQSQueueLifecycle covers the queue-management surface: URL shape,
// GetQueueUrl, ListQueues prefix filtering, idempotent vs conflicting
// re-create, SetQueueAttributes, attribute-name validation, and DeleteQueue
// tearing the queue down for its messages too.
func TestSQSQueueLifecycle(t *testing.T) {
	f := newSQSFixture(t, time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC))

	created := f.svcCall("CreateQueue", map[string]any{"QueueName": "alpha"})
	if created.Status != 200 || sqsBodyStr(created, "QueueUrl") != "http://sqs.stunt.test/alpha" {
		t.Fatalf("CreateQueue -> %d: %v", created.Status, created.Body)
	}
	if resp := f.svcCall("CreateQueue", map[string]any{"QueueName": "alphabet"}); resp.Status != 200 {
		t.Fatalf("CreateQueue alphabet -> %d: %v", resp.Status, resp.Body)
	}

	// GetQueueUrl round-trips; unknown queue is the real error.
	if resp := f.svcCall("GetQueueUrl", map[string]any{"QueueName": "alpha"}); resp.Status != 200 || sqsBodyStr(resp, "QueueUrl") != "http://sqs.stunt.test/alpha" {
		t.Fatalf("GetQueueUrl -> %d: %v", resp.Status, resp.Body)
	}
	if resp := f.svcCall("GetQueueUrl", map[string]any{"QueueName": "ghost"}); resp.Status != 400 || sqsErrType(t, resp) != "QueueDoesNotExist" {
		t.Fatalf("GetQueueUrl unknown -> %d %q, want 400 QueueDoesNotExist", resp.Status, sqsErrType(t, resp))
	}

	// ListQueues with and without prefix filtering.
	resp := f.svcCall("ListQueues", map[string]any{})
	urls, _ := resp.Body["QueueUrls"].([]any)
	if resp.Status != 200 || len(urls) != 2 {
		t.Fatalf("ListQueues -> %d: %v", resp.Status, resp.Body)
	}
	resp = f.svcCall("ListQueues", map[string]any{"QueueNamePrefix": "alphabet"})
	urls, _ = resp.Body["QueueUrls"].([]any)
	if len(urls) != 1 || urls[0] != "http://sqs.stunt.test/alphabet" {
		t.Fatalf("ListQueues prefix -> %v", resp.Body)
	}
	resp = f.svcCall("ListQueues", map[string]any{"QueueNamePrefix": "zzz"})
	if _, present := resp.Body["QueueUrls"]; present {
		t.Fatalf("ListQueues empty prefix returned urls: %v", resp.Body)
	}

	// Re-create with identical attributes is idempotent; a conflicting
	// VisibilityTimeout is QueueAlreadyExists.
	if resp := f.svcCall("CreateQueue", map[string]any{"QueueName": "alpha"}); resp.Status != 200 || sqsBodyStr(resp, "QueueUrl") != "http://sqs.stunt.test/alpha" {
		t.Fatalf("idempotent CreateQueue -> %d: %v", resp.Status, resp.Body)
	}
	resp = f.svcCall("CreateQueue", map[string]any{
		"QueueName":  "alpha",
		"Attributes": map[string]any{"VisibilityTimeout": "60"},
	})
	if resp.Status != 400 || sqsErrType(t, resp) != "QueueAlreadyExists" {
		t.Fatalf("conflicting CreateQueue -> %d %q, want 400 QueueAlreadyExists", resp.Status, sqsErrType(t, resp))
	}

	// SetQueueAttributes persists and shows up in GetQueueAttributes;
	// unknown names are rejected.
	if resp := f.svcCall("SetQueueAttributes", map[string]any{
		"QueueUrl":   "http://sqs.stunt.test/alpha",
		"Attributes": map[string]any{"VisibilityTimeout": "7"},
	}); resp.Status != 200 {
		t.Fatalf("SetQueueAttributes -> %d: %v", resp.Status, resp.Body)
	}
	resp = f.svcCall("GetQueueAttributes", map[string]any{
		"QueueUrl": "http://sqs.stunt.test/alpha", "AttributeNames": []string{"VisibilityTimeout"},
	})
	if resp.Status != 200 || resp.Body["Attributes"].(map[string]any)["VisibilityTimeout"] != "7" {
		t.Fatalf("GetQueueAttributes after set -> %d: %v", resp.Status, resp.Body)
	}
	resp = f.svcCall("GetQueueAttributes", map[string]any{
		"QueueUrl": "http://sqs.stunt.test/alpha", "AttributeNames": []string{"Nope"},
	})
	if resp.Status != 400 || sqsErrType(t, resp) != "InvalidAttributeName" {
		t.Fatalf("unknown attribute -> %d %q, want 400 InvalidAttributeName", resp.Status, sqsErrType(t, resp))
	}

	// DeleteQueue removes the queue; later sends hit QueueDoesNotExist.
	if resp := f.svcCall("DeleteQueue", map[string]any{"QueueUrl": "http://sqs.stunt.test/alphabet"}); resp.Status != 200 {
		t.Fatalf("DeleteQueue -> %d: %v", resp.Status, resp.Body)
	}
	resp = f.qCall("alphabet", "SendMessage", map[string]any{"MessageBody": "gone"})
	if resp.Status != 400 || sqsErrType(t, resp) != "QueueDoesNotExist" {
		t.Fatalf("send after delete -> %d %q, want 400 QueueDoesNotExist", resp.Status, sqsErrType(t, resp))
	}
}

func TestSQSThrottledProfile(t *testing.T) {
	// The adapter-authored 'throttled' mode: alternate receives return
	// empty (parity of a per-queue counter — deterministic, not chance).
	// VisibilityTimeout 0 keeps the message visible between receives so
	// only the throttling decides emptiness.
	start := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	throttled := false
	f := newSQSFixtureProfile(t, start, func() string {
		if throttled {
			return "throttled"
		}
		return ""
	})

	if resp := f.svcCall("CreateQueue", map[string]any{"QueueName": "jobs"}); resp.Status != 200 {
		t.Fatalf("CreateQueue -> %d: %v", resp.Status, resp.Body)
	}
	if resp := f.svcCall("SendMessage", map[string]any{
		"QueueUrl": "http://" + f.host + "/jobs", "MessageBody": "tick",
	}); resp.Status != 200 {
		t.Fatalf("SendMessage -> %d: %v", resp.Status, resp.Body)
	}
	recv := func() []any {
		resp := f.svcCall("ReceiveMessage", map[string]any{
			"QueueUrl": "http://" + f.host + "/jobs", "VisibilityTimeout": 0,
		})
		if resp.Status != 200 {
			t.Fatalf("ReceiveMessage -> %d: %v", resp.Status, resp.Body)
		}
		msgs, _ := resp.Body["Messages"].([]any)
		return msgs
	}

	// Baseline: without the profile every receive sees the message.
	if m := recv(); len(m) != 1 {
		t.Fatalf("baseline receive = %v, want the message", m)
	}
	throttled = true
	if m := recv(); len(m) != 1 { // counter 1: odd → served
		t.Fatalf("first throttled receive = %v, want the message", m)
	}
	if m := recv(); len(m) != 0 { // counter 2: even → empty
		t.Fatalf("second throttled receive = %v, want empty (throttled)", m)
	}
	if m := recv(); len(m) != 1 { // counter 3: odd → served again
		t.Fatalf("third throttled receive = %v, want the message", m)
	}
	throttled = false
	if m := recv(); len(m) != 1 {
		t.Fatalf("receive after deactivation = %v, want the message", m)
	}
}
