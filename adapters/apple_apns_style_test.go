package adapters

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the apple-apns-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the provider-token gate (real ES256
// verification with distinct reasons for missing/invalid/expired tokens),
// the POST /3/device/{token} push shape (200 + canonical-UUID apns-id,
// BadDeviceToken, Unregistered-shaped errors, PayloadEmpty) and the internal
// per-device notifications retrieval.
const (
	apnsVMHost       = "api.push.apple.test"
	apnsVMKnownToken = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	apnsVMNewToken   = "0000000000000000000000000000000000000000000000000000000000000000"
)

// apnsVMPrivPEM mirrors the adapter's documented synthetic provider keypair
// (README): the public half is baked into lib.star, the private half signs
// provider tokens here exactly the way a real APNs provider key would.
const apnsVMPrivPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgz399eDP4CEo1JoR7
A5uHueHShJKhKvna8BiAvVQPkvyhRANCAAS6OMBYKYI6moCMo0FeQ23CAvQMT5sy
MZrf7jMKmvhmI/aMJuodNWq4eLSq6/X4oWriaY7RsKxIrQ5F/Ql+y6XJ
-----END PRIVATE KEY-----`

// apnsVMUUID pins the canonical 8-4-4-4-12 hex apns-id real APNs returns.
var apnsVMUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type apnsVMFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vm   *starlark.VM
	host string
}

func newApnsVMFixture(t *testing.T, start time.Time) *apnsVMFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "apple-apns-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(root, "scripts", "send.star"))
	if err != nil {
		t.Fatalf("read send.star: %v", err)
	}
	tmp := t.TempDir()
	store, _ := primitives.Open(filepath.Join(tmp, "s.db"))
	t.Cleanup(func() { store.Close() })
	kvStore, _ := kv.Open(filepath.Join(tmp, "s.kv.db"))
	t.Cleanup(func() { kvStore.Close() })
	blobStore, _ := blob.Open(filepath.Join(tmp, "blobs"))
	t.Cleanup(func() { blobStore.Close() })

	vc := clock.NewVirtualClock(start)
	builtins := runtime.BuildAllBuiltins(runtime.BuiltinOptions{
		Store: store, KV: kvStore, Blob: blobStore, Clock: vc, ServiceName: "test", Emitter: events.NewEmitter(),
	})
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), builtins)
	if err != nil {
		t.Fatalf("LoadWithLib send.star: %v", err)
	}
	return &apnsVMFixture{t: t, vc: vc, vm: vm, host: apnsVMHost}
}

// call drives a handler on the device route; auth "" sends no Authorization
// header at all (the missing-token case).
func (f *apnsVMFixture) call(handler, method, path, token string, body map[string]any, auth string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	if auth != "" {
		headers["Authorization"] = auth
	}
	resp, err := f.vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: headers, Body: body,
		Params: map[string]string{"deviceToken": token},
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// send is the common push: an alert+badge aps payload to the given token.
func (f *apnsVMFixture) send(token, auth string) starlark.Response {
	f.t.Helper()
	return f.call("on_send", "POST", "/3/device/"+token, token, map[string]any{
		"aps": map[string]any{
			"alert": map[string]any{"title": "Test Push", "body": "Hello from stunt!"},
			"badge": 1,
		},
	}, auth)
}

// apnsVMSign signs a compact-JSON header/payload pair as a real ES256 JWT
// (raw r||s signature) with the given key.
func apnsVMSign(t *testing.T, priv *ecdsa.PrivateKey, header, payload string) string {
	t.Helper()
	h := base64.RawURLEncoding.EncodeToString([]byte(header))
	p := base64.RawURLEncoding.EncodeToString([]byte(payload))
	digest := sha256.Sum256([]byte(h + "." + p))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return h + "." + p + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// apnsVMKey parses the documented provider private key PEM.
func apnsVMKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode([]byte(apnsVMPrivPEM))
	if block == nil {
		t.Fatal("bad test key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("test key is not ECDSA")
	}
	return priv
}

// apnsVMMint mints a provider token: header {alg,kid}, payload
// {iss,iat[,exp]} — signed with the documented key.
func apnsVMMint(t *testing.T, priv *ecdsa.PrivateKey, iat, exp int64) string {
	t.Helper()
	header := `{"alg":"ES256","kid":"mock-apns-key-1"}`
	payload := `{"iss":"MOCKTEAMID","iat":` + strconv.FormatInt(iat, 10)
	if exp > 0 {
		payload += `,"exp":` + strconv.FormatInt(exp, 10)
	}
	return apnsVMSign(t, priv, header, payload+"}")
}

// wantReason asserts an APNs error envelope {reason: ...}.
func wantReason(t *testing.T, r starlark.Response, status int, reason, label string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s -> %d, want %d; body %v", label, r.Status, status, r.Body)
	}
	if r.Body["reason"] != reason {
		t.Fatalf("%s reason = %v, want %s", label, r.Body["reason"], reason)
	}
}

func TestAppleApnsVMProviderTokenGate(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newApnsVMFixture(t, base)
	priv := apnsVMKey(t)
	fresh := "Bearer " + apnsVMMint(t, priv, base.Unix(), 0)

	// ===== the provider token gate distinguishes missing invalid and expired tokens =====
	// No authorization header at all: real APNs reports MissingProviderToken.
	wantReason(t, f.send(apnsVMKnownToken, ""), 403, "MissingProviderToken", "no auth header")
	// Garbage bearer value.
	wantReason(t, f.send(apnsVMKnownToken, "Bearer not-a-jwt"), 403, "InvalidProviderToken", "garbage token")
	// A JWT whose JOSE header claims a symmetric alg.
	hs256 := "eyJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJNT0NLVEVBTUlEIn0.c2ln"
	wantReason(t, f.send(apnsVMKnownToken, "Bearer "+hs256), 403, "InvalidProviderToken", "HS256 alg")
	// A well-formed token signed by an unregistered key.
	forged, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	wantReason(t, f.send(apnsVMKnownToken, "Bearer "+apnsVMMint(t, forged, base.Unix(), 0)),
		403, "InvalidProviderToken", "forged signature")
	// iat 2h old (APNs caps provider tokens at 1h when exp is absent).
	wantReason(t, f.send(apnsVMKnownToken, "Bearer "+apnsVMMint(t, priv, base.Unix()-7200, 0)),
		403, "ExpiredProviderToken", "stale iat")
	// An explicit exp in the past expires the token.
	wantReason(t, f.send(apnsVMKnownToken, "Bearer "+apnsVMMint(t, priv, base.Unix()-7200, base.Unix()-3600)),
		403, "ExpiredProviderToken", "explicit past exp")
	// A fresh iat (exp absent, within the 1h cap) goes through.
	if r := f.send(apnsVMKnownToken, fresh); r.Status != 200 {
		t.Fatalf("fresh provider token -> %d, want 200; body %v", r.Status, r.Body)
	}
}

func TestAppleApnsVMDevicePush(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newApnsVMFixture(t, base)
	auth := "Bearer " + apnsVMMint(t, apnsVMKey(t), base.Unix(), 0)

	// ===== a push to the known device returns 200 with a canonical uuid apns-id =====
	first := f.send(apnsVMKnownToken, auth)
	if first.Status != 200 {
		t.Fatalf("push known device -> %d: %v", first.Status, first.Body)
	}
	apnsID := first.Headers["apns-id"]
	if !apnsVMUUID.MatchString(apnsID) {
		t.Fatalf("apns-id = %q, want canonical 8-4-4-4-12 UUID", apnsID)
	}
	if len(first.Body) != 0 {
		t.Fatalf("push body = %v, want empty body on 200", first.Body)
	}
	// A second push gets a different id.
	second := f.send(apnsVMKnownToken, auth)
	if second.Status != 200 {
		t.Fatalf("second push -> %d: %v", second.Status, second.Body)
	}
	apnsID2 := second.Headers["apns-id"]
	if apnsID2 == "" || apnsID2 == apnsID {
		t.Fatalf("second apns-id = %q, want distinct from %q", apnsID2, apnsID)
	}

	// ===== unknown device tokens are 400 baddevicetoken =====
	wantReason(t, f.send(apnsVMNewToken, auth), 400, "BadDeviceToken", "unknown device")

	// ===== empty aps payloads are 400 payloadempty =====
	// A nil body, a body without aps, and an aps with no alert/badge/sound.
	if r := f.call("on_send", "POST", "/3/device/"+apnsVMKnownToken, apnsVMKnownToken, nil, auth); r.Status != 400 || r.Body["reason"] != "PayloadEmpty" {
		t.Fatalf("nil body -> %d %v, want 400 PayloadEmpty", r.Status, r.Body)
	}
	if r := f.call("on_send", "POST", "/3/device/"+apnsVMKnownToken, apnsVMKnownToken,
		map[string]any{"custom": map[string]any{"x": 1}}, auth); r.Status != 400 || r.Body["reason"] != "PayloadEmpty" {
		t.Fatalf("no aps -> %d %v, want 400 PayloadEmpty", r.Status, r.Body)
	}
	if r := f.call("on_send", "POST", "/3/device/"+apnsVMKnownToken, apnsVMKnownToken,
		map[string]any{"aps": map[string]any{}}, auth); r.Status != 400 || r.Body["reason"] != "PayloadEmpty" {
		t.Fatalf("empty aps -> %d %v, want 400 PayloadEmpty", r.Status, r.Body)
	}
	// A sound-only aps is a valid minimal payload.
	if r := f.call("on_send", "POST", "/3/device/"+apnsVMKnownToken, apnsVMKnownToken,
		map[string]any{"aps": map[string]any{"sound": "chime.aiff"}}, auth); r.Status != 200 {
		t.Fatalf("sound-only aps -> %d, want 200; body %v", r.Status, r.Body)
	}

	// ===== sent notifications are retrievable per device =====
	list := f.call("on_get_notifications", "GET", "/3/device/"+apnsVMKnownToken+"/notifications",
		apnsVMKnownToken, nil, auth)
	if list.Status != 200 {
		t.Fatalf("notifications -> %d: %v", list.Status, list.Body)
	}
	items := list.BodyList
	if len(items) != 3 {
		t.Fatalf("notifications count = %d, want 3 (two pushes + sound-only)", len(items))
	}
	ids := map[string]bool{}
	for _, it := range items {
		n, _ := it.(map[string]any)
		ids[n["apns-id"].(string)] = true
		if _, has := n["aps"]; !has {
			t.Fatalf("notification %v missing aps", n)
		}
		if _, has := n["sent_at"]; !has {
			t.Fatalf("notification %v missing sent_at", n)
		}
	}
	if !ids[apnsID] || !ids[apnsID2] {
		t.Fatalf("notification ids %v missing the served apns-ids %q / %q", ids, apnsID, apnsID2)
	}
	// The unknown device has nothing stored.
	if r := f.call("on_get_notifications", "GET", "/3/device/"+apnsVMNewToken+"/notifications",
		apnsVMNewToken, nil, auth); r.Status != 200 || len(r.BodyList) != 0 {
		t.Fatalf("unknown device notifications -> %d %v, want 200 []", r.Status, r.BodyList)
	}
	// The gate applies to the internal route too.
	wantReason(t, f.call("on_get_notifications", "GET", "/3/device/"+apnsVMKnownToken+"/notifications",
		apnsVMKnownToken, nil, ""), 403, "MissingProviderToken", "notifications without auth")
}
