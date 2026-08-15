package engine

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// CloudKit server-to-server signing material, matching the documented
// synthetic keypair in adapters/cloudkit-style (README + scripts/lib.star):
// the PRIVATE half signs requests here; the adapter verifies with the public
// half. KeyID must equal the adapter's _CK_KEY_ID constant.
const ckKeyID = "stunt-cloudkit-s2s-key-1"

const ckPrivateKeyPEM = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgYWuBd8XWfDZ/VcJu
QB09aJCel9cxSAjTK0x6bsCiCVGhRANCAAQ6HcT9YUUVXeqvZzOGGORZ89rQX0Ne
n8el83/HqrrAlhhMFWpHo3iuSuqqFdhgd9XBSPPM9+E2RK/+qy+C4Qiw
-----END PRIVATE KEY-----`

// ckSignHeaders computes the three CloudKit request-signature headers for a
// request to rawurl with the given raw body bytes, exactly the way a real
// client does: message = date + ":" + rawBody + ":" + path, signed with
// ECDSA P-256 + SHA256, base64-encoded raw r||s.
func ckSignHeaders(t *testing.T, date, rawurl string, rawBody []byte) map[string]string {
	t.Helper()
	block, _ := pem.Decode([]byte(ckPrivateKeyPEM))
	if block == nil {
		t.Fatal("bad test private key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse test private key: %v", err)
	}
	priv, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("test key is not ECDSA")
	}
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	msg := date + ":" + string(rawBody) + ":" + u.Path
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
	if err != nil {
		t.Fatalf("ecdsa sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return map[string]string{
		"X-Apple-CloudKit-Request-KeyID":           ckKeyID,
		"X-Apple-CloudKit-Request-ISO8601Date":     date,
		"X-Apple-CloudKit-Request-SignatureBase64": base64.StdEncoding.EncodeToString(sig),
	}
}

// TestCloudKitStyleAdapter exercises the cloudkit-style adapter:
//
//   - Auth required: 401 without signed CloudKit request headers
//   - Lookup records → seeded records
//   - Modify (create) → new record appears
//   - Modify (update) → field updated
//   - Query by recordType → filtered results
//   - Zones list
//   - Current user
//
// Every authenticated request is signed for real (ECDSA P-256 over
// date:body:path with the documented synthetic keypair), so the whole flow
// runs through the adapter's verification path.
func TestCloudKitStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "cloudkit-style")
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
			"cloudkit": {Adapter: absAdapterDir},
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

	base := addrs["cloudkit"]
	prefix := base + "/database/1/com.example.icloud-container/production/public"

	// ===== 401 without auth (no signature headers at all) =====

	body, status := cloudKitGetJSONUnsigned(t, prefix+"/records/lookup",
		map[string]any{"records": []map[string]any{{"recordName": "note-001"}}},
		nil)
	if status != 401 {
		t.Fatalf("lookup without auth -> status %d, want 401; body %s", status, body)
	}

	// ===== Lookup → seeded record =====

	body, status = cloudKitGetJSON(t, prefix+"/records/lookup",
		map[string]any{"records": []map[string]any{{"recordName": "note-001"}}})
	if status != 200 {
		t.Fatalf("lookup -> status %d, want 200; body %s", status, body)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	records, ok := resp["records"].([]any)
	if !ok || len(records) != 1 {
		t.Fatalf("records = %v, want array of 1", resp["records"])
	}
	rec := records[0].(map[string]any)
	if rec["recordName"] != "note-001" {
		t.Fatalf("recordName = %v, want note-001", rec["recordName"])
	}
	if rec["recordType"] != "Notes" {
		t.Fatalf("recordType = %v, want Notes", rec["recordType"])
	}
	fields, ok := rec["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields = %v, want object", rec["fields"])
	}
	titleField, ok := fields["title"].(map[string]any)
	if !ok {
		t.Fatalf("fields.title = %v, want object", fields["title"])
	}
	if titleField["value"] != "Welcome Note" {
		t.Fatalf("title value = %v, want 'Welcome Note'", titleField["value"])
	}

	// ===== Modify (create) → new record =====

	body, status = cloudKitPostJSON(t, prefix+"/records/modify", map[string]any{
		"operations": []map[string]any{
			{
				"operationType": "create",
				"record": map[string]any{
					"recordName": "note-003",
					"recordType": "Notes",
					"fields": map[string]any{
						"title": map[string]any{"value": "New Note"},
						"body":  map[string]any{"value": "Created via modify"},
					},
				},
			},
		},
	})
	if status != 200 {
		t.Fatalf("modify (create) -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal modify: %v (body %s)", err, body)
	}
	records = resp["records"].([]any)
	rec = records[0].(map[string]any)
	if rec["recordName"] != "note-003" {
		t.Fatalf("created recordName = %v, want note-003", rec["recordName"])
	}

	// ===== Lookup the created record (STATEFUL) =====

	body, status = cloudKitGetJSON(t, prefix+"/records/lookup",
		map[string]any{"records": []map[string]any{{"recordName": "note-003"}}})
	if status != 200 {
		t.Fatalf("lookup created -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, body)
	}
	records = resp["records"].([]any)
	rec = records[0].(map[string]any)
	if rec["recordName"] != "note-003" {
		t.Fatalf("lookup created recordName = %v, want note-003", rec["recordName"])
	}

	// ===== Query by recordType → filtered results =====

	body, status = cloudKitGetJSON(t, prefix+"/records/query", map[string]any{
		"query": map[string]any{
			"recordType": "Notes",
		},
	})
	if status != 200 {
		t.Fatalf("query -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal query: %v (body %s)", err, body)
	}
	records = resp["records"].([]any)
	if len(records) < 2 {
		t.Fatalf("query results count = %d, want >= 2 (seeded + created)", len(records))
	}

	// ===== Zones list =====

	body, status = cloudKitGet(t, prefix+"/zones/list")
	if status != 200 {
		t.Fatalf("zones -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal zones: %v (body %s)", err, body)
	}
	zones, ok := resp["zones"].([]any)
	if !ok || len(zones) < 1 {
		t.Fatalf("zones = %v, want non-empty array", resp["zones"])
	}

	// ===== Current user =====

	body, status = cloudKitGet(t, prefix+"/users/current")
	if status != 200 {
		t.Fatalf("current user -> status %d, want 200; body %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal user: %v (body %s)", err, body)
	}
	if resp["userRecordName"] != "_owner" {
		t.Fatalf("userRecordName = %v, want _owner", resp["userRecordName"])
	}
}

// TestCloudKitStyleRequestSignature drives the CloudKit request-signature
// verification's negative paths: a correctly signed request passes, while a
// stale date header, an unknown KeyID, a tampered body, and a garbage
// signature all get 401 AUTHENTICATION_FAILED.
func TestCloudKitStyleRequestSignature(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "cloudkit-style")
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
			"cloudkit": {Adapter: absAdapterDir},
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

	prefix := addrs["cloudkit"] + "/database/1/com.example.icloud-container/production/public"
	lookup := map[string]any{"records": []map[string]any{{"recordName": "note-001"}}}
	raw, _ := json.Marshal(lookup)
	target := prefix + "/records/lookup"

	// Correct signature → 200.
	h := ckSignHeaders(t, time.Now().UTC().Format(time.RFC3339), target, raw)
	body, status := cloudKitGetJSONUnsigned(t, target, lookup, h)
	if status != 200 {
		t.Fatalf("signed request -> %d, want 200; body %s", status, body)
	}

	// Stale date (outside the 10-minute window) → 401.
	stale := time.Now().UTC().Add(-20 * time.Minute).Format(time.RFC3339)
	h = ckSignHeaders(t, stale, target, raw)
	body, status = cloudKitGetJSONUnsigned(t, target, lookup, h)
	if status != 401 {
		t.Fatalf("stale date -> %d, want 401; body %s", status, body)
	}

	// Unknown KeyID → 401.
	h = ckSignHeaders(t, time.Now().UTC().Format(time.RFC3339), target, raw)
	h["X-Apple-CloudKit-Request-KeyID"] = "someone-elses-key"
	body, status = cloudKitGetJSONUnsigned(t, target, lookup, h)
	if status != 401 {
		t.Fatalf("unknown key id -> %d, want 401; body %s", status, body)
	}

	// Tampered body (signature no longer covers the raw bytes) → 401.
	h = ckSignHeaders(t, time.Now().UTC().Format(time.RFC3339), target, raw)
	tampered := map[string]any{"records": []map[string]any{{"recordName": "note-002"}}}
	body, status = cloudKitGetJSONUnsigned(t, target, tampered, h)
	if status != 401 {
		t.Fatalf("tampered body -> %d, want 401; body %s", status, body)
	}

	// Garbage signature (valid base64, not a signature for this message) → 401.
	h = ckSignHeaders(t, time.Now().UTC().Format(time.RFC3339), target, raw)
	junk := make([]byte, 64)
	for i := range junk {
		junk[i] = byte(i)
	}
	h["X-Apple-CloudKit-Request-SignatureBase64"] = base64.StdEncoding.EncodeToString(junk)
	body, status = cloudKitGetJSONUnsigned(t, target, lookup, h)
	if status != 401 {
		t.Fatalf("garbage signature -> %d, want 401; body %s", status, body)
	}

	// Non-base64 signature → 401 (not a 500: the adapter pre-validates).
	h = ckSignHeaders(t, time.Now().UTC().Format(time.RFC3339), target, raw)
	h["X-Apple-CloudKit-Request-SignatureBase64"] = "!!!not-base64!!!"
	body, status = cloudKitGetJSONUnsigned(t, target, lookup, h)
	if status != 401 {
		t.Fatalf("non-base64 signature -> %d, want 401; body %s", status, body)
	}
}

// === CloudKit test helpers ===
//
// The cloudKitGet/GetJSON/PostJSON trio signs every request with the
// documented synthetic keypair; the Unsigned variants pass the headers
// through verbatim (nil = no auth headers) for the failure paths.

func cloudKitGet(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	raw := []byte("{}")
	return cloudKitDo(t, "GET", rawurl, raw, ckSignHeaders(t, time.Now().UTC().Format(time.RFC3339), rawurl, raw))
}

func cloudKitGetJSON(t *testing.T, rawurl string, body map[string]any) (string, int) {
	t.Helper()
	raw, _ := json.Marshal(body)
	return cloudKitDo(t, "GET", rawurl, raw, ckSignHeaders(t, time.Now().UTC().Format(time.RFC3339), rawurl, raw))
}

func cloudKitGetJSONUnsigned(t *testing.T, rawurl string, body map[string]any, headers map[string]string) (string, int) {
	t.Helper()
	raw, _ := json.Marshal(body)
	return cloudKitDo(t, "GET", rawurl, raw, headers)
}

func cloudKitPostJSON(t *testing.T, rawurl string, body map[string]any) (string, int) {
	t.Helper()
	raw, _ := json.Marshal(body)
	return cloudKitDo(t, "POST", rawurl, raw, ckSignHeaders(t, time.Now().UTC().Format(time.RFC3339), rawurl, raw))
}

func cloudKitDo(t *testing.T, method, rawurl string, data []byte, headers map[string]string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(method, rawurl, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}
