package adapters

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	sk "go.starlark.net/starlark"

	"stuntapi.com/stunt/internal/adapter/runtime"
	"stuntapi.com/stunt/internal/primitives"
	"stuntapi.com/stunt/internal/primitives/blob"
	"stuntapi.com/stunt/internal/primitives/clock"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// The azure-storage-style adapter documents these synthetic credentials (see
// its README "SharedKey verification"); the tests derive the same MACs in Go.
const (
	storageDemoAccount = "stuntstorage"
	storageDemoKeyRaw  = "stunt-local-storage-signing-key"
)

// storageHarness opens one set of backing stores (so state persists across
// handler files) with a virtual clock pinned to fixed.
type storageHarness struct {
	t        *testing.T
	fixed    time.Time
	builtins sk.StringDict
}

func newStorageHarness(t *testing.T, fixed time.Time) *storageHarness {
	t.Helper()
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
	return &storageHarness{
		t:     t,
		fixed: fixed,
		builtins: runtime.BuildAllBuiltins(runtime.BuiltinOptions{
			Store:       store,
			KV:          kvStore,
			Blob:        blobStore,
			Clock:       clock.NewVirtualClock(fixed),
			ServiceName: "test",
		}),
	}
}

// vm loads a handler from the azure-storage-style adapter with its lib.star
// preloaded, sharing the harness stores.
func (h *storageHarness) vm(handlerRel string) *starlark.VM {
	h.t.Helper()
	dir := repoAdaptersDir(h.t)
	root := filepath.Join(dir, "azure-storage-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		h.t.Fatalf("read lib.star: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(root, handlerRel))
	if err != nil {
		h.t.Fatalf("read %s: %v", handlerRel, err)
	}
	vm, err := starlark.LoadWithLib(string(src), string(libSrc), h.builtins)
	if err != nil {
		h.t.Fatalf("LoadWithLib: %v", err)
	}
	return vm
}

// storageSTS mirrors the adapter's SharedKey string-to-sign construction
// (2015-02-21+ form). Header keys must be lowercase, like the engine passes.
func storageSTS(method, path, account string, headers, query map[string]string) string {
	// Normalize to lowercase keys, like the engine's case-insensitive
	// req.headers.
	h := make(map[string]string, len(headers))
	for k, v := range headers {
		h[strings.ToLower(k)] = v
	}
	cl := h["content-length"]
	if cl == "0" {
		cl = ""
	}
	lines := []string{
		method,
		h["content-encoding"], h["content-language"], cl,
		h["content-md5"], h["content-type"], h["date"],
		h["if-modified-since"], h["if-match"], h["if-none-match"],
		h["if-unmodified-since"], h["range"],
	}
	sts := strings.Join(lines, "\n") + "\n"

	var xms []string
	for k, v := range h {
		if strings.HasPrefix(k, "x-ms-") {
			xms = append(xms, k+":"+strings.TrimSpace(v))
		}
	}
	sort.Strings(xms)
	for _, h := range xms {
		sts += h + "\n"
	}

	sts += "/" + account + path
	var keys []string
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sts += "\n" + k + ":" + query[k]
	}
	return sts
}

// storageSign returns the base64 SharedKey signature for the request.
func storageSign(method, path, account string, headers, query map[string]string) string {
	mac := hmac.New(sha256.New, []byte(storageDemoKeyRaw))
	mac.Write([]byte(storageSTS(method, path, account, headers, query)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func storageAuth(account, sig string) string {
	return "SharedKey " + account + ":" + sig
}

// storageReq builds a signed starlark.Request.
func storageReq(t *testing.T, method, path string, headers, query map[string]string, params map[string]string, rawBody string) starlark.Request {
	t.Helper()
	signed := map[string]string{}
	for k, v := range headers {
		signed[strings.ToLower(k)] = v
	}
	signed["Authorization"] = storageAuth(storageDemoAccount, storageSign(method, path, storageDemoAccount, signed, query))
	return starlark.Request{
		Method:  method,
		Path:    path,
		Headers: signed,
		Query:   query,
		Params:  params,
		RawBody: rawBody,
	}
}

// TestAzureStorageSharedKeyRoundTrip is the positive path: correctly signed
// container create, blob upload, download, and ListContainers all pass auth,
// and Last-Modified reflects the (virtual) clock instead of a hardcoded date.
func TestAzureStorageSharedKeyRoundTrip(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	h := newStorageHarness(t, fixed)
	vm := h.vm(filepath.Join("scripts", "blobs.star"))
	contVM := h.vm(filepath.Join("scripts", "containers.star"))
	httpDate := fixed.Format(http.TimeFormat)

	// Create the container (PUT /mycontainer).
	hdr := map[string]string{
		"content-length": "0",
		"x-ms-date":      httpDate,
		"x-ms-version":   "2024-08-04",
	}
	resp, err := contVM.Call("on_create_container", storageReq(t, "PUT", "/mycontainer", hdr, nil, map[string]string{"container": "mycontainer"}, ""))
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	if resp.Status != 201 {
		t.Fatalf("create container status = %d, want 201", resp.Status)
	}
	if got := resp.Headers["Last-Modified"]; got != httpDate {
		t.Errorf("create container Last-Modified = %q, want %q (clock-driven, not hardcoded)", got, httpDate)
	}
	if got := resp.Headers["ETag"]; !strings.HasPrefix(got, `"0x`) {
		t.Errorf("create container ETag = %q, want quoted 0x-prefixed etag", got)
	}

	// Upload a blob (PUT /mycontainer/report.json) with byte-exact content.
	body := `{"report":{"rows":[1,2,3]}}`
	hdr = map[string]string{
		"content-length": strconv.Itoa(len(body)),
		"content-type":   "application/json",
		"x-ms-blob-type": "BlockBlob",
		"x-ms-date":      httpDate,
		"x-ms-version":   "2024-08-04",
	}
	resp, err = vm.Call("on_put_blob", storageReq(t, "PUT", "/mycontainer/report.json", hdr, nil, map[string]string{"container": "mycontainer", "blob": "report.json"}, body))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	if resp.Status != 201 {
		t.Fatalf("put blob status = %d, want 201", resp.Status)
	}
	etag := resp.Headers["ETag"]
	if etag == "" || etag == `""` {
		t.Fatalf("put blob ETag missing: %q", etag)
	}
	if got := resp.Headers["Last-Modified"]; got != httpDate {
		t.Errorf("put blob Last-Modified = %q, want %q", got, httpDate)
	}

	// Download the blob (GET) — bytes round-trip, ETag/Last-Modified come
	// from the stored content.
	hdr = map[string]string{
		"x-ms-date":    httpDate,
		"x-ms-version": "2024-08-04",
	}
	resp, err = vm.Call("on_get_blob", storageReq(t, "GET", "/mycontainer/report.json", hdr, nil, map[string]string{"container": "mycontainer", "blob": "report.json"}, ""))
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("get blob status = %d, want 200", resp.Status)
	}
	if resp.RawBody != body {
		t.Errorf("get blob body = %q, want %q", resp.RawBody, body)
	}
	if resp.Headers["ETag"] != etag {
		t.Errorf("get blob ETag = %q, want %q (real etag from content)", resp.Headers["ETag"], etag)
	}
	if resp.Headers["Last-Modified"] != httpDate {
		t.Errorf("get blob Last-Modified = %q, want %q", resp.Headers["Last-Modified"], httpDate)
	}

	// ListContainers (GET /?comp=list) — the canonicalized resource includes
	// the sorted query params.
	resp, err = contVM.Call("on_list_containers", storageReq(t, "GET", "/", map[string]string{
		"x-ms-date":    httpDate,
		"x-ms-version": "2024-08-04",
	}, map[string]string{"comp": "list"}, nil, ""))
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("list containers status = %d, want 200", resp.Status)
	}
	if !strings.Contains(resp.RawBody, "<Name>mycontainer</Name>") {
		t.Errorf("list containers XML missing container: %q", resp.RawBody)
	}
}

// TestAzureStorageSharedKeyTampered is the negative path: a signature that
// does not match the string-to-sign is rejected with the real Azure 403
// AuthenticationFailed XML envelope.
func TestAzureStorageSharedKeyTampered(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	h := newStorageHarness(t, fixed)
	vm := h.vm(filepath.Join("scripts", "blobs.star"))

	body := "hello"
	hdr := map[string]string{
		"content-length": strconv.Itoa(len(body)),
		"x-ms-date":      fixed.Format(http.TimeFormat),
		"x-ms-version":   "2024-08-04",
	}
	req := storageReq(t, "PUT", "/mycontainer/tampered.txt", hdr, nil, map[string]string{"container": "mycontainer", "blob": "tampered.txt"}, body)
	sig := storageSign("PUT", "/mycontainer/tampered.txt", storageDemoAccount, req.Headers, nil)
	tampered := sig
	if strings.HasPrefix(sig, "A") {
		tampered = "B" + sig[1:]
	} else {
		tampered = "A" + sig[1:]
	}
	req.Headers["Authorization"] = storageAuth(storageDemoAccount, tampered)

	resp, err := vm.Call("on_put_blob", req)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Status != 403 {
		t.Fatalf("status = %d, want 403", resp.Status)
	}
	if !strings.Contains(resp.RawBody, "AuthenticationFailed") || !strings.Contains(resp.RawBody, "<Error>") {
		t.Errorf("error envelope = %q, want Azure AuthenticationFailed XML", resp.RawBody)
	}
}

// TestAzureStorageSharedKeyUnknownAccount verifies an account that is not in
// the documented key table fails closed.
func TestAzureStorageSharedKeyUnknownAccount(t *testing.T) {
	fixed := time.Unix(1_750_000_000, 0).UTC()
	h := newStorageHarness(t, fixed)
	vm := h.vm(filepath.Join("scripts", "containers.star"))

	hdr := map[string]string{"x-ms-date": fixed.Format(http.TimeFormat)}
	signed := map[string]string{"x-ms-date": hdr["x-ms-date"]}
	// Sign with the demo key but claim a different account: the MAC input
	// uses that account in the canonicalized resource either way.
	sig := storageSign("PUT", "/other", "nosuchaccount", signed, nil)
	req := starlark.Request{
		Method:  "PUT",
		Path:    "/other",
		Headers: map[string]string{"x-ms-date": hdr["x-ms-date"], "Authorization": storageAuth("nosuchaccount", sig)},
		Params:  map[string]string{"container": "other"},
	}
	resp, err := vm.Call("on_create_container", req)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Status != 403 {
		t.Fatalf("status = %d, want 403", resp.Status)
	}
	if !strings.Contains(resp.RawBody, "AuthenticationFailed") {
		t.Errorf("error envelope = %q, want AuthenticationFailed", resp.RawBody)
	}
}
