package adapters

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
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

// Drives the cloudkit-style adapter scripts directly (lib.star preloaded)
// over a shared store and virtual clock: the server-to-server request
// signature gate (real ECDSA P-256 over date:raw_body:path), and the five
// public-database routes — users/current, zones/list, records/lookup,
// records/query and records/modify — against the seeded zones and Notes.
const ckVMKeyID = "stunt-cloudkit-s2s-key-1"

// ckVMPrivPEM mirrors the adapter's documented synthetic server-to-server
// keypair (README): the private half signs here exactly the way a real
// CloudKit web-services client would.
const ckVMPrivPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgYWuBd8XWfDZ/VcJu
QB09aJCel9cxSAjTK0x6bsCiCVGhRANCAAQ6HcT9YUUVXeqvZzOGGORZ89rQX0Ne
n8el83/HqrrAlhhMFWpHo3iuSuqqFdhgd9XBSPPM9+E2RK/+qy+C4Qiw
-----END PRIVATE KEY-----`

// ckVMPrefix is the public-database path every route hangs off (the
// container/env path params are opaque to the handlers).
const ckVMPrefix = "/database/1/iCloud.stunt.test/production/public"

type ckVMFixture struct {
	t    *testing.T
	vc   *clock.Clock
	vms  map[string]*starlark.VM
	host string
}

func newCloudKitVMFixture(t *testing.T, start time.Time) *ckVMFixture {
	t.Helper()
	root := filepath.Join(repoAdaptersDir(t), "cloudkit-style")
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
	return &ckVMFixture{t: t, vc: vc, host: "api.apple-cloudkit.test", vms: map[string]*starlark.VM{
		"records": load("records.star"), "zones": load("zones.star"), "users": load("users.star"),
	}}
}

// signed computes the three X-Apple-CloudKit-Request-* headers over
// date:rawBody:path for the marshaled body (ECDSA P-256 + SHA-256, base64
// raw r||s) — the client side of the adapter's documented scheme.
func (f *ckVMFixture) signed(path string, body map[string]any, date time.Time) map[string]string {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("marshal sign body: %v", err)
	}
	block, _ := pem.Decode([]byte(ckVMPrivPEM))
	if block == nil {
		f.t.Fatal("bad test private key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		f.t.Fatalf("parse test key: %v", err)
	}
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		f.t.Fatal("test key is not ECDSA")
	}
	dateStr := date.UTC().Format(time.RFC3339)
	msg := dateStr + ":" + string(raw) + ":" + path
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
	if err != nil {
		f.t.Fatalf("ecdsa sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return map[string]string{
		"X-Apple-CloudKit-Request-KeyID":           ckVMKeyID,
		"X-Apple-CloudKit-Request-ISO8601Date":     dateStr,
		"X-Apple-CloudKit-Request-SignatureBase64": base64.StdEncoding.EncodeToString(sig),
	}
}

// call drives handler on the named script VM with a JSON body: the raw bytes
// feed raw_body (the signature covers them verbatim) and the re-parsed map
// feeds body — what the engine hands a handler. hdrs nil = correctly signed
// for this body at the virtual clock's now.
func (f *ckVMFixture) call(group, handler, method, path string, body map[string]any, hdrs map[string]string) starlark.Response {
	f.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatalf("marshal body: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		f.t.Fatalf("re-parse body: %v", err)
	}
	if hdrs == nil {
		hdrs = f.signed(path, body, f.vc.Now())
	}
	resp, err := f.vms[group].Call(handler, starlark.Request{
		Method: method, Path: path, Host: f.host, Headers: hdrs, Body: parsed, RawBody: string(raw),
	})
	if err != nil {
		f.t.Fatalf("%s %s: %v", handler, path, err)
	}
	return resp
}

// ckVMNum coerces a JSON number that may surface as int64 or float64 (the
// backing store round-trips records as JSON) to float64 for comparison.
func ckVMNum(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return -1
	}
}

// wantAuthFailed asserts the CloudKit auth-failure envelope.
func wantAuthFailed(t *testing.T, r starlark.Response, label string) {
	t.Helper()
	if r.Status != 401 {
		t.Fatalf("%s -> %d, want 401; body %v", label, r.Status, r.Body)
	}
	if r.Body["serverErrorCode"] != "AUTHENTICATION_FAILED" {
		t.Fatalf("%s serverErrorCode = %v, want AUTHENTICATION_FAILED", label, r.Body["serverErrorCode"])
	}
}

func TestCloudKitVMRequestSignatureGate(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newCloudKitVMFixture(t, base)
	lookup := ckVMPrefix + "/records/lookup"
	body := map[string]any{"records": []any{map[string]any{"recordName": "note-001"}}}

	// ===== the s2s signature gate rejects unsigned tampered stale and foreign-key requests =====
	// Unsigned (no X-Apple-CloudKit-Request-* headers at all) -> 401.
	wantAuthFailed(t, f.call("records", "on_lookup", "GET", lookup, body, map[string]string{}), "unsigned")

	// Unknown KeyID (signed correctly, but not the registered key id).
	h := f.signed(lookup, body, f.vc.Now())
	h["X-Apple-CloudKit-Request-KeyID"] = "someone-elses-key"
	wantAuthFailed(t, f.call("records", "on_lookup", "GET", lookup, body, h), "unknown key id")

	// Tampered body: the signature covers note-001's raw bytes, note-002 ships.
	h = f.signed(lookup, body, f.vc.Now())
	other := map[string]any{"records": []any{map[string]any{"recordName": "note-002"}}}
	wantAuthFailed(t, f.call("records", "on_lookup", "GET", lookup, other, h), "tampered body")

	// Garbage signature (valid base64, wrong bytes).
	h = f.signed(lookup, body, f.vc.Now())
	junk := make([]byte, 64)
	for i := range junk {
		junk[i] = byte(i)
	}
	h["X-Apple-CloudKit-Request-SignatureBase64"] = base64.StdEncoding.EncodeToString(junk)
	wantAuthFailed(t, f.call("records", "on_lookup", "GET", lookup, body, h), "garbage signature")

	// Stale date: 20 minutes off the (virtual) server clock is outside the
	// 10-minute window.
	wantAuthFailed(t, f.call("records", "on_lookup", "GET", lookup, body,
		f.signed(lookup, body, f.vc.Now().Add(-20*time.Minute))), "stale date")

	// Inside the window (9 minutes old) still passes.
	if r := f.call("records", "on_lookup", "GET", lookup, body,
		f.signed(lookup, body, f.vc.Now().Add(-9*time.Minute))); r.Status != 200 {
		t.Fatalf("9-minute-old date -> %d, want 200; body %v", r.Status, r.Body)
	}
}

func TestCloudKitVMDatabaseRoutes(t *testing.T) {
	base := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	f := newCloudKitVMFixture(t, base)

	// ===== users current returns the s2s owner identity =====
	user := f.call("users", "on_current_user", "GET", ckVMPrefix+"/users/current", nil, nil)
	if user.Status != 200 {
		t.Fatalf("users/current -> %d: %v", user.Status, user.Body)
	}
	if user.Body["userRecordName"] != "_owner" {
		t.Fatalf("userRecordName = %v, want _owner", user.Body["userRecordName"])
	}
	if user.Body["firstName"] != "Test" || user.Body["lastName"] != "User" {
		t.Fatalf("user identity = %v %v, want Test User", user.Body["firstName"], user.Body["lastName"])
	}

	// ===== zones list seeds defaults filters by prefix and pages =====
	zones := f.call("zones", "on_list_zones", "GET", ckVMPrefix+"/zones/list", nil, nil)
	if zones.Status != 200 {
		t.Fatalf("zones/list -> %d: %v", zones.Status, zones.Body)
	}
	zoneList, _ := zones.Body["zones"].([]any)
	if len(zoneList) != 2 {
		t.Fatalf("seeded zones = %d, want 2 (%v)", len(zoneList), zones.Body)
	}
	first, _ := zoneList[0].(map[string]any)
	if first["zoneName"] != "_default" || first["zoneType"] != "DEFAULT_ZONE" {
		t.Fatalf("first zone = %v, want _default DEFAULT_ZONE", first)
	}
	// zoneNamePrefix filters before paging.
	prefixed := f.call("zones", "on_list_zones", "GET", ckVMPrefix+"/zones/list",
		map[string]any{"zoneNamePrefix": "_own"}, nil)
	pl, _ := prefixed.Body["zones"].([]any)
	if len(pl) != 1 || pl[0].(map[string]any)["zoneName"] != "_owner" {
		t.Fatalf("prefix _own zones = %v, want only _owner", prefixed.Body)
	}
	// resultsLimit pages through the seeded zones with a continuation marker
	// (JSON number, the shape real CloudKit documents).
	page1 := f.call("zones", "on_list_zones", "GET", ckVMPrefix+"/zones/list",
		map[string]any{"resultsLimit": float64(1)}, nil)
	if page1.Status != 200 {
		t.Fatalf("zones page 1 -> %d: %v", page1.Status, page1.Body)
	}
	p1, _ := page1.Body["zones"].([]any)
	if len(p1) != 1 || p1[0].(map[string]any)["zoneName"] != "_default" {
		t.Fatalf("zones page 1 = %v, want just _default", page1.Body)
	}
	marker, has := page1.Body["continuationMarker"]
	if !has || marker == "" {
		t.Fatalf("zones page 1 continuationMarker = %v, want non-empty", page1.Body["continuationMarker"])
	}
	page2 := f.call("zones", "on_list_zones", "GET", ckVMPrefix+"/zones/list",
		map[string]any{"resultsLimit": float64(1), "continuationMarker": marker}, nil)
	p2, _ := page2.Body["zones"].([]any)
	if len(p2) != 1 || p2[0].(map[string]any)["zoneName"] != "_owner" {
		t.Fatalf("zones page 2 = %v, want just _owner", page2.Body)
	}
	if _, has := page2.Body["continuationMarker"]; has {
		t.Fatalf("zones page 2 continuationMarker = %v, want absent (exhausted)", page2.Body["continuationMarker"])
	}

	// ===== records lookup returns the seeded shape with inline NOT_FOUND =====
	look := f.call("records", "on_lookup", "GET", ckVMPrefix+"/records/lookup",
		map[string]any{"records": []any{
			map[string]any{"recordName": "note-001"},
			map[string]any{"recordName": "no-such-record"},
		}}, nil)
	if look.Status != 200 {
		t.Fatalf("records/lookup -> %d: %v", look.Status, look.Body)
	}
	recs, _ := look.Body["records"].([]any)
	if len(recs) != 2 {
		t.Fatalf("lookup records = %d, want 2", len(recs))
	}
	rec, _ := recs[0].(map[string]any)
	if rec["recordName"] != "note-001" || rec["recordType"] != "Notes" {
		t.Fatalf("lookup record = %v, want note-001 Notes", rec)
	}
	fields, _ := rec["fields"].(map[string]any)
	title, _ := fields["title"].(map[string]any)
	if title["value"] != "Welcome Note" {
		t.Fatalf("note-001 title = %v, want Welcome Note", title["value"])
	}
	created, _ := rec["created"].(map[string]any)
	if ts := ckVMNum(created["timestamp"]); ts != 1700000000000 {
		t.Fatalf("note-001 created.timestamp = %v, want 1700000000000", created["timestamp"])
	}
	// A missing name yields an inline per-record error, not a failed request.
	missing, _ := recs[1].(map[string]any)
	if missing["serverErrorCode"] != "NOT_FOUND" {
		t.Fatalf("missing record entry = %v, want inline NOT_FOUND", missing)
	}

	// ===== records query filters sorts and pages on a numeric resultsLimit =====
	eq := f.call("records", "on_query", "GET", ckVMPrefix+"/records/query", map[string]any{
		"query": map[string]any{
			"recordType": "Notes",
			"filterBy":   []any{map[string]any{"fieldName": "title", "comparator": "EQUALS", "fieldValue": map[string]any{"value": "Welcome Note"}}},
		},
	}, nil)
	if eq.Status != 200 {
		t.Fatalf("query EQUALS -> %d: %v", eq.Status, eq.Body)
	}
	if eqRecs, _ := eq.Body["records"].([]any); len(eqRecs) != 1 {
		t.Fatalf("query EQUALS records = %d, want 1 (%v)", len(eqRecs), eq.Body)
	}
	bw := f.call("records", "on_query", "GET", ckVMPrefix+"/records/query", map[string]any{
		"query": map[string]any{
			"recordType": "Notes",
			"filterBy":   []any{map[string]any{"fieldName": "title", "comparator": "BEGINS_WITH", "fieldValue": map[string]any{"value": "Shopping"}}},
		},
	}, nil)
	if bwRecs, _ := bw.Body["records"].([]any); len(bwRecs) != 1 ||
		bwRecs[0].(map[string]any)["recordName"] != "note-002" {
		t.Fatalf("query BEGINS_WITH = %v, want note-002 only", bw.Body)
	}
	// sortBy descending on title puts "Welcome Note" before "Shopping List".
	sorted := f.call("records", "on_query", "GET", ckVMPrefix+"/records/query", map[string]any{
		"query": map[string]any{
			"recordType": "Notes",
			"sortBy":     []any{map[string]any{"fieldName": "title", "ascending": false}},
		},
	}, nil)
	sRecs, _ := sorted.Body["records"].([]any)
	if len(sRecs) != 2 {
		t.Fatalf("query sorted records = %d, want 2", len(sRecs))
	}
	s0 := sRecs[0].(map[string]any)["fields"].(map[string]any)["title"].(map[string]any)["value"]
	s1 := sRecs[1].(map[string]any)["fields"].(map[string]any)["title"].(map[string]any)["value"]
	if s0 != "Welcome Note" || s1 != "Shopping List" {
		t.Fatalf("sortBy title desc = [%v %v], want [Welcome Note Shopping List]", s0, s1)
	}
	// Page through both notes one at a time following continuationMarker.
	pages, marker := 0, ""
	for {
		qb := map[string]any{"query": map[string]any{"recordType": "Notes"}, "resultsLimit": float64(1)}
		if marker != "" {
			qb["continuationMarker"] = marker
		}
		page := f.call("records", "on_query", "GET", ckVMPrefix+"/records/query", qb, nil)
		if page.Status != 200 {
			t.Fatalf("query page %d -> %d: %v", pages+1, page.Status, page.Body)
		}
		pRecs, _ := page.Body["records"].([]any)
		if len(pRecs) != 1 {
			t.Fatalf("query page %d records = %d, want 1", pages+1, len(pRecs))
		}
		pages++
		m, has := page.Body["continuationMarker"]
		if !has {
			break
		}
		marker, _ = m.(string)
	}
	if pages != 2 {
		t.Fatalf("query pages = %d, want 2", pages)
	}
	// A syntactically invalid marker is the adapter's 400, not a 500.
	if r := f.call("records", "on_query", "GET", ckVMPrefix+"/records/query", map[string]any{
		"query": map[string]any{"recordType": "Notes"}, "continuationMarker": "bogus",
	}, nil); r.Status != 400 {
		t.Fatalf("invalid continuationMarker -> %d, want 400; body %v", r.Status, r.Body)
	}

	// ===== records modify creates updates and deletes round-trip =====
	mod := f.call("records", "on_modify", "POST", ckVMPrefix+"/records/modify", map[string]any{
		"operations": []any{map[string]any{
			"operationType": "create",
			"record": map[string]any{
				"recordName": "note-003", "recordType": "Notes",
				"fields": map[string]any{
					"title": map[string]any{"value": "New Note"},
					"body":  map[string]any{"value": "Created via modify"},
				},
			},
		}},
	}, nil)
	if mod.Status != 200 {
		t.Fatalf("modify create -> %d: %v", mod.Status, mod.Body)
	}
	mRecs, _ := mod.Body["records"].([]any)
	if len(mRecs) != 1 || mRecs[0].(map[string]any)["recordName"] != "note-003" {
		t.Fatalf("modify create records = %v, want note-003", mod.Body)
	}
	// The created record reads back through lookup (stateful).
	back := f.call("records", "on_lookup", "GET", ckVMPrefix+"/records/lookup",
		map[string]any{"records": []any{map[string]any{"recordName": "note-003"}}}, nil)
	bRecs, _ := back.Body["records"].([]any)
	if len(bRecs) != 1 {
		t.Fatalf("lookup note-003 = %v", back.Body)
	}
	if got := bRecs[0].(map[string]any)["fields"].(map[string]any)["title"].(map[string]any)["value"]; got != "New Note" {
		t.Fatalf("note-003 title = %v, want New Note", got)
	}
	// Update merges fields (body untouched) and bumps modified, not created.
	upd := f.call("records", "on_modify", "POST", ckVMPrefix+"/records/modify", map[string]any{
		"operations": []any{map[string]any{
			"operationType": "update",
			"record": map[string]any{
				"recordName": "note-003",
				"fields":     map[string]any{"title": map[string]any{"value": "Renamed"}},
			},
		}},
	}, nil)
	uRec, _ := upd.Body["records"].([]any)[0].(map[string]any)
	uFields := uRec["fields"].(map[string]any)
	if uFields["title"].(map[string]any)["value"] != "Renamed" {
		t.Fatalf("updated title = %v, want Renamed", uFields["title"])
	}
	if uFields["body"].(map[string]any)["value"] != "Created via modify" {
		t.Fatalf("updated body = %v, want merged original", uFields["body"])
	}
	if ckVMNum(uRec["modified"].(map[string]any)["timestamp"]) <= ckVMNum(uRec["created"].(map[string]any)["timestamp"]) {
		t.Fatalf("modified %v <= created %v", uRec["modified"], uRec["created"])
	}
	// forceUpdate of a missing record is an inline NOT_FOUND entry.
	miss := f.call("records", "on_modify", "POST", ckVMPrefix+"/records/modify", map[string]any{
		"operations": []any{map[string]any{
			"operationType": "forceUpdate",
			"record":        map[string]any{"recordName": "ghost", "fields": map[string]any{}},
		}},
	}, nil)
	if e := miss.Body["records"].([]any)[0].(map[string]any)["serverErrorCode"]; e != "NOT_FOUND" {
		t.Fatalf("forceUpdate ghost = %v, want inline NOT_FOUND", e)
	}
	// Unknown operation types are reported inline, not fatal.
	bad := f.call("records", "on_modify", "POST", ckVMPrefix+"/records/modify", map[string]any{
		"operations": []any{map[string]any{"operationType": "frobnicate"}},
	}, nil)
	if e := bad.Body["records"].([]any)[0].(map[string]any)["serverErrorCode"]; e != "BAD_REQUEST" {
		t.Fatalf("unknown operation = %v, want inline BAD_REQUEST", e)
	}
	// Delete removes the record and reports {recordName, deleted}.
	del := f.call("records", "on_modify", "POST", ckVMPrefix+"/records/modify", map[string]any{
		"operations": []any{map[string]any{
			"operationType": "delete", "record": map[string]any{"recordName": "note-003"},
		}},
	}, nil)
	dRec, _ := del.Body["records"].([]any)[0].(map[string]any)
	if dRec["deleted"] != true || dRec["recordName"] != "note-003" {
		t.Fatalf("delete record = %v, want {note-003, deleted}", dRec)
	}
	gone := f.call("records", "on_lookup", "GET", ckVMPrefix+"/records/lookup",
		map[string]any{"records": []any{map[string]any{"recordName": "note-003"}}}, nil)
	if e := gone.Body["records"].([]any)[0].(map[string]any)["serverErrorCode"]; e != "NOT_FOUND" {
		t.Fatalf("lookup deleted note-003 = %v, want NOT_FOUND", e)
	}
}
