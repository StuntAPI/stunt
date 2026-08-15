package adapters

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// The azure-servicebus-style adapter documents these synthetic SAS
// credentials (see its README "SAS verification"); the tests derive the same
// MACs in Go.
const (
	sbKeyName   = "stuntkey"
	sbKeySecret = "stunt-servicebus-signing-key"
)

// sbVM loads a handler from the azure-servicebus-style adapter with its
// lib.star preloaded and a virtual clock pinned to fixed.
func sbVM(t *testing.T, handlerRel string, fixed time.Time) *starlark.VM {
	t.Helper()
	dir := repoAdaptersDir(t)
	root := filepath.Join(dir, "azure-servicebus-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(root, handlerRel))
	if err != nil {
		t.Fatalf("read %s: %v", handlerRel, err)
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
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store:       store,
		KV:          kvStore,
		Blob:        blobStore,
		Clock:       clock.NewVirtualClock(fixed),
		ServiceName: "test",
	})
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
	if err != nil {
		t.Fatalf("LoadWithLib: %v", err)
	}
	return vm
}

// sbSign returns base64url(HMAC-SHA256(secret, resource + "\n" + expiry)),
// the real Service Bus SAS string-to-sign.
func sbSign(resource string, expiry int64) string {
	mac := hmac.New(sha256.New, []byte(sbKeySecret))
	mac.Write([]byte(resource + "\n" + strconv.FormatInt(expiry, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// sbToken assembles the Authorization header value for a SAS token.
func sbToken(resource string, expiry int64, keyName, sig string) string {
	return fmt.Sprintf("SharedAccessSignature sr=%s&sig=%s&se=%d&skn=%s",
		resource, sig, expiry, keyName)
}

// sbReq builds a request carrying the given Authorization header.
func sbReq(auth string, body map[string]any, params map[string]string) starlark.Request {
	return starlark.Request{
		Method:  "POST",
		Path:    "/queue1/messages",
		Headers: map[string]string{"Authorization": auth},
		Body:    body,
		Params:  params,
	}
}

// errCode digs error.code out of an ARM-style envelope.
func errCode(t *testing.T, resp starlark.Response) string {
	t.Helper()
	e, ok := resp.Body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error envelope: %+v", resp.Body)
	}
	code, _ := e["code"].(string)
	return code
}

// TestServiceBusSASPositive is the positive path: a correctly signed SAS
// token (sr + "\n" + se, base64url HMAC-SHA256 with the documented key)
// passes auth and the message is sent.
func TestServiceBusSASPositive(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	vm := sbVM(t, filepath.Join("scripts", "servicebus.star"), fixed)

	sr := "https://stunt.servicebus.windows.net/queue1"
	se := fixed.Add(time.Hour).Unix()
	auth := sbToken(sr, se, sbKeyName, sbSign(sr, se))

	resp, err := vm.Call("on_queue_messages", sbReq(auth,
		map[string]any{"Body": "hello", "ContentType": "application/json"},
		map[string]string{"queue": "queue1"}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.Status != 201 {
		t.Fatalf("send status = %d, want 201", resp.Status)
	}
	if resp.Body["MessageId"] == "" || resp.Body["LockToken"] == "" {
		t.Errorf("send response missing ids: %+v", resp.Body)
	}
}

// TestServiceBusSASTampered verifies a wrong signature is rejected with the
// real ASB 401 InvalidSignature code.
func TestServiceBusSASTampered(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	vm := sbVM(t, filepath.Join("scripts", "servicebus.star"), fixed)

	sr := "https://stunt.servicebus.windows.net/queue1"
	se := fixed.Add(time.Hour).Unix()
	sig := sbSign(sr, se)
	if len(sig) == 0 || sig[0] == 'A' {
		sig = "B" + sig[1:]
	} else {
		sig = "A" + sig[1:]
	}

	resp, err := vm.Call("on_queue_messages", sbReq(sbToken(sr, se, sbKeyName, sig), nil, map[string]string{"queue": "queue1"}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.Status != 401 {
		t.Fatalf("status = %d, want 401", resp.Status)
	}
	if code := errCode(t, resp); code != "InvalidSignature" {
		t.Errorf("error code = %q, want InvalidSignature", code)
	}
}

// TestServiceBusSASExpired verifies an expired se (checked against
// clock.now_unix) is rejected with the real ASB 401 ExpiredToken code.
func TestServiceBusSASExpired(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	vm := sbVM(t, filepath.Join("scripts", "servicebus.star"), fixed)

	sr := "https://stunt.servicebus.windows.net/queue1"
	se := fixed.Add(-time.Minute).Unix() // signed correctly, but in the past

	resp, err := vm.Call("on_queue_messages", sbReq(sbToken(sr, se, sbKeyName, sbSign(sr, se)), nil, map[string]string{"queue": "queue1"}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.Status != 401 {
		t.Fatalf("status = %d, want 401", resp.Status)
	}
	if code := errCode(t, resp); code != "ExpiredToken" {
		t.Errorf("error code = %q, want ExpiredToken", code)
	}
}

// TestServiceBusSASMalformed verifies structural problems (missing skn) are
// rejected with the real ASB 401 MalformedToken code, and an unknown key
// name with 401 InvalidSignature.
func TestServiceBusSASMalformed(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	vm := sbVM(t, filepath.Join("scripts", "servicebus.star"), fixed)

	sr := "https://stunt.servicebus.windows.net/queue1"
	se := fixed.Add(time.Hour).Unix()
	sig := sbSign(sr, se)

	cases := []struct {
		name string
		auth string
		want string
	}{
		{"missing skn", fmt.Sprintf("SharedAccessSignature sr=%s&sig=%s&se=%d", sr, sig, se), "MalformedToken"},
		{"missing se", fmt.Sprintf("SharedAccessSignature sr=%s&sig=%s&skn=%s", sr, sig, sbKeyName), "MalformedToken"},
		{"unknown key name", sbToken(sr, se, "otherkey", sig), "InvalidSignature"},
		{"bad scheme", "Basic dXNlcjpwYXNz", "MalformedToken"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := vm.Call("on_queue_messages", sbReq(tc.auth, nil, map[string]string{"queue": "queue1"}))
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			if resp.Status != 401 {
				t.Fatalf("status = %d, want 401", resp.Status)
			}
			if code := errCode(t, resp); code != tc.want {
				t.Errorf("error code = %q, want %q", code, tc.want)
			}
		})
	}
}

// TestServiceBusBearerAccepted verifies Bearer tokens still pass.
func TestServiceBusBearerAccepted(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	vm := sbVM(t, filepath.Join("scripts", "servicebus.star"), fixed)

	resp, err := vm.Call("on_queue_messages", sbReq("Bearer some-entra-token",
		map[string]any{"Body": "hi"}, map[string]string{"queue": "queue1"}))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.Status != 201 {
		t.Fatalf("status = %d, want 201", resp.Status)
	}
}

// TestServiceBusSASOnStorageQueue covers the storage-queue endpoints, which
// share the same SAS verifier via lib.star.
func TestServiceBusSASOnStorageQueue(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	vm := sbVM(t, filepath.Join("scripts", "storage.star"), fixed)

	sr := "https://stuntaccount.queue.core.windows.net/q9"
	se := fixed.Add(time.Hour).Unix()
	auth := sbToken(sr, se, sbKeyName, sbSign(sr, se))

	resp, err := vm.Call("on_send_storage_message", starlark.Request{
		Method:  "POST",
		Path:    "/stuntaccount/q9/messages",
		Headers: map[string]string{"Authorization": auth},
		Body:    map[string]any{"MessageText": "ping"},
		Params:  map[string]string{"account": "stuntaccount", "queue": "q9"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.Status != 201 {
		t.Fatalf("send status = %d, want 201 (body %q)", resp.Status, resp.RawBody)
	}

	// Tamper the signature: 401 InvalidSignature.
	tampered := sbToken(sr, se, sbKeyName, "AAAA"+sbSign(sr, se)[4:])
	resp, err = vm.Call("on_send_storage_message", starlark.Request{
		Method:  "POST",
		Path:    "/stuntaccount/q9/messages",
		Headers: map[string]string{"Authorization": tampered},
		Body:    map[string]any{"MessageText": "ping"},
		Params:  map[string]string{"account": "stuntaccount", "queue": "q9"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if resp.Status != 401 || errCode(t, resp) != "InvalidSignature" {
		t.Fatalf("status = %d code = %q, want 401 InvalidSignature", resp.Status, errCode(t, resp))
	}
}
