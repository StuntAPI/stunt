package adapters

import (
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
	"stuntapi.com/stunt/internal/primitives/events"
	"stuntapi.com/stunt/internal/primitives/kv"
	"stuntapi.com/stunt/internal/starlark"
)

// Drives the pinata-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the API-key-pair / Bearer-JWT
// gate, content-addressed CIDv0 pinning (JSON + multipart file) with
// isDuplicate re-pin detection, the pinList filters and default-10 paging,
// pinByHash, unpin, and the {error:{reason,details}} envelope.
var (
	pinBearer  = map[string]string{"Authorization": "Bearer pinata-jwt"}
	pinKeyPair = map[string]string{"pinata_api_key": "vm-key", "pinata_secret_api_key": "vm-secret"}
)

// Expected CIDs: base58(0x12 0x20 || sha256(content)) — real CIDv0 math,
// so identical bytes always pin to the identical "Qm…" hash.
const (
	pinJSONHash = "QmYGx7Wzqe5prvEsTSzYBQN8xViYUM9qsWJSF5EENLcNmM" // {"hello":"world"}
)

type pinataFixture struct {
	t      *testing.T
	vc     *clock.Clock
	vmPin  *starlark.VM
	vmData *starlark.VM
}

func newPinataFixture(t *testing.T, start time.Time) *pinataFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "pinata-style")
	libSrc, err := os.ReadFile(filepath.Join(root, "scripts", "lib.star"))
	if err != nil {
		t.Fatalf("read lib.star: %v", err)
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
	return &pinataFixture{t: t, vc: vc, vmPin: load("pinning.star"), vmData: load("data.star")}
}

// call invokes handler on vm with explicit headers (nil = header absent).
func (f *pinataFixture) call(vm *starlark.VM, handler, method, path string, params, query map[string]string, body map[string]any, headers map[string]string) starlark.Response {
	f.t.Helper()
	resp, err := vm.Call(handler, starlark.Request{
		Method: method, Path: path, Host: "api.pinata.cloud",
		Headers: headers, Body: body, Params: params, Query: query,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// callMultipart is call with a raw multipart body, the way the engine
// delivers file uploads (JSON body nil, bytes in raw_body).
func (f *pinataFixture) callMultipart(handler, path, contentType, rawBody string) starlark.Response {
	f.t.Helper()
	headers := map[string]string{}
	for k, v := range pinBearer {
		headers[k] = v
	}
	headers["Content-Type"] = contentType
	resp, err := f.vmPin.Call(handler, starlark.Request{
		Method: "POST", Path: path, Host: "api.pinata.cloud", Headers: headers, RawBody: rawBody,
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// pinRows pulls the rows array out of a {count, rows} envelope.
func pinRows(t *testing.T, r starlark.Response) []any {
	t.Helper()
	rows, ok := r.Body["rows"].([]any)
	if !ok {
		t.Fatalf("rows = %v, want an array (status %d)", r.Body["rows"], r.Status)
	}
	return rows
}

// pinErrReason extracts error.reason from the Pinata error envelope.
func pinErrReason(r starlark.Response) string {
	e, _ := r.Body["error"].(map[string]any)
	reason, _ := e["reason"].(string)
	return reason
}

// pinataMultipart builds a file + pinataMetadata multipart body.
func pinataMultipart(boundary, filename, fileData, metaJSON string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=%q\r\nContent-Type: application/octet-stream\r\n\r\n%s\r\n",
		boundary, filename, fileData)
	fmt.Fprintf(&b, "--%s\r\nContent-Disposition: form-data; name=\"pinataMetadata\"\r\nContent-Type: application/json\r\n\r\n%s\r\n", boundary, metaJSON)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.String()
}

func TestPinataPinningLifecycle(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newPinataFixture(t, base)

	// ===== missing or half-present credentials are 401 with the error envelope =====
	// No credentials, key without secret, and an empty Bearer all 401.
	for name, headers := range map[string]map[string]string{
		"no credentials":     nil,
		"key without secret": {"pinata_api_key": "vm-key"},
		"bearer no token":    {"Authorization": "Bearer "},
	} {
		r := f.call(f.vmData, "on_test_auth", "GET", "/data/testAuthentication", nil, nil, nil, headers)
		if r.Status != 401 || pinErrReason(r) != "UNAUTHORIZED" {
			t.Fatalf("%s -> %d %v, want 401 UNAUTHORIZED", name, r.Status, r.Body)
		}
	}

	// ===== the API key pair and a Bearer JWT both open testAuthentication =====
	// Either scheme yields the congratulations message.
	for name, headers := range map[string]map[string]string{"key pair": pinKeyPair, "bearer": pinBearer} {
		r := f.call(f.vmData, "on_test_auth", "GET", "/data/testAuthentication", nil, nil, nil, headers)
		if r.Status != 200 || r.Body["message"] != "Congratulations! You are communicating with the Pinata API!" {
			t.Fatalf("%s testAuthentication -> %d %v", name, r.Status, r.Body)
		}
	}

	// ===== pinJSONToIPFS pins content to a real CIDv0 =====
	// Size is the serialized content's byte length; the CID is real CIDv0
	// (base58 of the sha2-256 multihash), stamped at request time.
	jsonPin := f.call(f.vmPin, "on_pin_json", "POST", "/pinning/pinJSONToIPFS", nil, nil, map[string]any{
		"pinataContent":  map[string]any{"hello": "world"},
		"pinataMetadata": map[string]any{"name": "vm-pin"},
	}, pinBearer)
	if jsonPin.Status != 200 {
		t.Fatalf("pinJSONToIPFS -> %d: %v", jsonPin.Status, jsonPin.Body)
	}
	if jsonPin.Body["IpfsHash"] != pinJSONHash {
		t.Fatalf("IpfsHash = %v, want the content's CIDv0 %s", jsonPin.Body["IpfsHash"], pinJSONHash)
	}
	if jsonPin.Body["PinSize"] != int64(17) { // len(`{"hello":"world"}`)
		t.Fatalf("PinSize = %v, want 17 (serialized bytes)", jsonPin.Body["PinSize"])
	}
	if jsonPin.Body["Timestamp"] != "2026-02-03T12:00:00.000Z" || jsonPin.Body["isDuplicate"] != false {
		t.Fatalf("pin result = %v, want request-time Timestamp and isDuplicate false", jsonPin.Body)
	}
	// A body-less pin is a 400.
	if r := f.call(f.vmPin, "on_pin_json", "POST", "/pinning/pinJSONToIPFS", nil, nil, nil, pinBearer); r.Status != 400 || pinErrReason(r) != "BAD_REQUEST" {
		t.Fatalf("body-less pinJSONToIPFS -> %d %v, want 400 BAD_REQUEST", r.Status, r.Body)
	}

	// ===== re-pinning identical content is isDuplicate, not a new pin =====
	// Same bytes -> same CID, isDuplicate true, and no second row.
	dup := f.call(f.vmPin, "on_pin_json", "POST", "/pinning/pinJSONToIPFS", nil, nil, map[string]any{
		"pinataContent": map[string]any{"hello": "world"},
	}, pinKeyPair)
	if dup.Status != 200 || dup.Body["IpfsHash"] != pinJSONHash || dup.Body["isDuplicate"] != true {
		t.Fatalf("re-pin -> %d %v, want same CID and isDuplicate true", dup.Status, dup.Body)
	}
	other := f.call(f.vmPin, "on_pin_json", "POST", "/pinning/pinJSONToIPFS", nil, nil, map[string]any{
		"pinataContent": map[string]any{"hello": "stunt"},
	}, pinBearer)
	if other.Status != 200 || other.Body["IpfsHash"] == pinJSONHash || other.Body["isDuplicate"] != false {
		t.Fatalf("different content -> %d %v, want a fresh CID", other.Status, other.Body)
	}

	// ===== pinFileToIPFS sizes and names the pin from the multipart parts =====
	// The file part's bytes set PinSize/CID; pinataMetadata names it. A
	// multipart body with no file part is a 400.
	f.vc.Advance(30 * time.Minute)
	mp := pinataMultipart("vmBoundary", "hello.bin", strings.Repeat("ipfs", 500), `{"name":"vm-file-pin"}`)
	filePin := f.callMultipart("on_pin_file", "/pinning/pinFileToIPFS",
		"multipart/form-data; boundary=vmBoundary", mp)
	if filePin.Status != 200 || filePin.Body["PinSize"] != int64(2000) || filePin.Body["isDuplicate"] != false {
		t.Fatalf("pinFileToIPFS -> %d %v, want PinSize 2000 from the file part", filePin.Status, filePin.Body)
	}
	fileHash, _ := filePin.Body["IpfsHash"].(string)
	if !strings.HasPrefix(fileHash, "Qm") || len(fileHash) != 46 {
		t.Fatalf("file IpfsHash = %q, want a 46-char CIDv0", fileHash)
	}
	if filePin.Body["Timestamp"] != "2026-02-03T12:30:00.000Z" {
		t.Fatalf("file Timestamp = %v, want the advanced clock time", filePin.Body["Timestamp"])
	}
	metaOnly := "--vmB\r\nContent-Disposition: form-data; name=\"pinataMetadata\"\r\n\r\n{}\r\n--vmB--\r\n"
	if r := f.callMultipart("on_pin_file", "/pinning/pinFileToIPFS", "multipart/form-data; boundary=vmB", metaOnly); r.Status != 400 || pinErrReason(r) != "BAD_REQUEST" {
		t.Fatalf("file-less pinFileToIPFS -> %d %v, want 400 BAD_REQUEST", r.Status, r.Body)
	}

	// ===== pinList filters by hash, size, status, and metadata name =====
	// Three rows so far: two JSON pins (17/18 bytes, 12:00) and the file
	// (2000 bytes, 12:30). Every filter narrows within them.
	list := func(query map[string]string) starlark.Response {
		return f.call(f.vmData, "on_pin_list", "GET", "/data/pinList", nil, query, nil, pinBearer)
	}
	if r := list(map[string]string{"hashContains": pinJSONHash}); r.Status != 200 || r.Body["count"] != int64(1) {
		t.Fatalf("hashContains -> %d %v, want the one matching row", r.Status, r.Body)
	}
	if r := list(map[string]string{"pinSizeMin": "1000"}); r.Body["count"] != int64(1) {
		t.Fatalf("pinSizeMin 1000 -> %v, want only the 2000-byte file", r.Body["count"])
	}
	if r := list(map[string]string{"pinSizeMax": "1000"}); r.Body["count"] != int64(2) {
		t.Fatalf("pinSizeMax 1000 -> %v, want the two JSON pins", r.Body["count"])
	}
	if r := list(map[string]string{"status": "unpinned"}); r.Status != 200 || r.Body["count"] != int64(0) || len(pinRows(t, r)) != 0 {
		t.Fatalf("status unpinned -> %d %v, want an empty result (no unpin history)", r.Status, r.Body)
	}
	if r := list(map[string]string{"status": "all"}); r.Body["count"] != int64(3) {
		t.Fatalf("status all -> %v, want all three pins", r.Body["count"])
	}
	meta := list(map[string]string{"metadata": `{"name":"vm-file-pin"}`})
	if meta.Status != 200 || meta.Body["count"] != int64(1) {
		t.Fatalf("metadata name filter -> %d %v, want the file pin", meta.Status, meta.Body)
	}
	if mrows := pinRows(t, meta); len(mrows) == 1 {
		row, _ := mrows[0].(map[string]any)
		md, _ := row["metadata"].(map[string]any)
		if md["name"] != "vm-file-pin" || row["ipfs_pin_hash"] != fileHash {
			t.Fatalf("metadata-filtered row = %v, want the named file pin", mrows[0])
		}
	}

	// ===== pinStart/pinEnd bound the date-pinned window =====
	// ISO-8601 bounds: before 12:15 keeps the JSON pins, after 12:15 the file.
	if r := list(map[string]string{"pinEnd": "2026-02-03T12:15:00.000Z"}); r.Body["count"] != int64(2) {
		t.Fatalf("pinEnd 12:15 -> %v, want the two 12:00 JSON pins", r.Body["count"])
	}
	if r := list(map[string]string{"pinStart": "2026-02-03T12:15:00.000Z"}); r.Body["count"] != int64(1) {
		t.Fatalf("pinStart 12:15 -> %v, want only the 12:30 file pin", r.Body["count"])
	}
	if r := list(map[string]string{"pinStart": "2026-06-01T00:00:00.000Z", "pinEnd": "2026-06-02T00:00:00.000Z"}); r.Body["count"] != int64(0) {
		t.Fatalf("empty date window -> %v, want 0", r.Body["count"])
	}

	// ===== pinByHash requires hash and matches the CID exactly =====
	byHash := func(query map[string]string) starlark.Response {
		return f.call(f.vmData, "on_pin_by_hash", "GET", "/data/pinByHash", nil, query, nil, pinBearer)
	}
	if r := byHash(map[string]string{"hash": fileHash}); r.Status != 200 || r.Body["count"] != int64(1) || len(pinRows(t, r)) != 1 {
		t.Fatalf("pinByHash file -> %d %v, want exactly the file pin", r.Status, r.Body)
	}
	if r := byHash(map[string]string{"hash": "QmNotAPinnedCid0000000000000000000000000000"}); r.Status != 200 || r.Body["count"] != int64(0) {
		t.Fatalf("pinByHash unknown -> %d %v, want an empty 200", r.Status, r.Body)
	}
	if r := byHash(nil); r.Status != 400 || pinErrReason(r) != "BAD_REQUEST" {
		t.Fatalf("pinByHash without hash -> %d %v, want 400 BAD_REQUEST", r.Status, r.Body)
	}

	// ===== pinList pages at the real default of 10 rows =====
	// 12 more JSON pins -> 15 rows; default page 10, count stays 15.
	f.vc.Advance(time.Hour)
	for i := 1; i <= 12; i++ {
		r := f.call(f.vmPin, "on_pin_json", "POST", "/pinning/pinJSONToIPFS", nil, nil, map[string]any{
			"pinataContent": map[string]any{"i": i},
		}, pinBearer)
		if r.Status != 200 || r.Body["isDuplicate"] != false {
			t.Fatalf("pin %d -> %d %v, want a fresh pin", i, r.Status, r.Body)
		}
	}
	page1 := list(nil)
	if rows := pinRows(t, page1); len(rows) != 10 || page1.Body["count"] != int64(15) {
		t.Fatalf("default pinList -> %d rows count %v, want 10 of 15", len(rows), page1.Body["count"])
	}
	page2 := list(map[string]string{"pageOffset": "10"})
	if rows := pinRows(t, page2); len(rows) != 5 || page2.Body["count"] != int64(15) {
		t.Fatalf("pageOffset 10 -> %d rows count %v, want the remaining 5 of 15", len(rows), page2.Body["count"])
	}
	tail := list(map[string]string{"pageLimit": "3", "pageOffset": "12"})
	if rows := pinRows(t, tail); len(rows) != 3 || tail.Body["count"] != int64(15) {
		t.Fatalf("pageLimit 3 offset 12 -> %d rows, want 3", len(rows))
	}
	id1, _ := pinRows(t, page1)[0].(map[string]any)["id"].(string)
	id2, _ := pinRows(t, page2)[0].(map[string]any)["id"].(string)
	if id1 == id2 {
		t.Fatalf("paging repeated row %s", id1)
	}

	// ===== unpin removes the CID; a second unpin is 403 FORBIDDEN =====
	// Unpin 200s once, empties pinByHash, drops the list count, then 403s.
	unpin := f.call(f.vmPin, "on_unpin", "DELETE", "/pinning/unpin/"+fileHash,
		map[string]string{"cid": fileHash}, nil, nil, pinBearer)
	if unpin.Status != 200 {
		t.Fatalf("unpin -> %d: %v", unpin.Status, unpin.Body)
	}
	if r := byHash(map[string]string{"hash": fileHash}); r.Status != 200 || r.Body["count"] != int64(0) {
		t.Fatalf("pinByHash after unpin -> %d %v, want 0 rows", r.Status, r.Body)
	}
	if r := list(map[string]string{"hashContains": fileHash}); r.Body["count"] != int64(0) {
		t.Fatalf("pinList after unpin -> %v, want the file pin gone", r.Body["count"])
	}
	if r := list(nil); r.Body["count"] != int64(14) {
		t.Fatalf("pinList count after unpin = %v, want 14", r.Body["count"])
	}
	if r := f.call(f.vmPin, "on_unpin", "DELETE", "/pinning/unpin/"+fileHash,
		map[string]string{"cid": fileHash}, nil, nil, pinBearer); r.Status != 403 || pinErrReason(r) != "FORBIDDEN" {
		t.Fatalf("second unpin -> %d %v, want 403 FORBIDDEN", r.Status, r.Body)
	}
	if r := f.call(f.vmPin, "on_unpin", "DELETE", "/pinning/unpin/QmNotAPinnedCid0000000000000000000000000000",
		map[string]string{"cid": "QmNotAPinnedCid0000000000000000000000000000"}, nil, nil, pinKeyPair); r.Status != 403 {
		t.Fatalf("unpin unknown CID -> %d, want 403", r.Status)
	}
}
