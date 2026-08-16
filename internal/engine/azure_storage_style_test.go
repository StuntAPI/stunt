package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// TestAzureStorageStyleAdapter exercises the Azure Blob Storage-style adapter:
//
//   - Create container (PUT)
//   - PUT blob with SharedKey auth → 201
//   - ListBlobs (XML) shows the uploaded blob (STATEFUL)
//   - GET blob returns content
//   - HEAD blob returns metadata headers
//   - DELETE blob → 202
//   - SAS token query form works (no Authorization header)
//   - ListContainers (XML)
//   - Blob metadata get/set
//   - Without auth → 401 error XML
//   - Malformed SharedKey → 403
func TestAzureStorageStyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "azure-storage-style")
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
			"azure": {Adapter: absAdapterDir},
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

	base := addrs["azure"]

	// Fake but structurally valid SharedKey.
	const sharedKey = "SharedKey stuntstorage:dHVudA=="

	// ===== Create container =====

	body, status := azPut(t, base+"/mycontainer", sharedKey, nil)
	if status != 201 {
		t.Fatalf("create container -> status %d, want 201; body %s", status, body)
	}

	// ===== Upload blob with SharedKey =====

	uploadContent := `{"hello":"azure"}`
	body, status = azPut(t, base+"/mycontainer/test.json", sharedKey, []byte(uploadContent))
	if status != 201 {
		t.Fatalf("put blob -> status %d, want 201; body %s", status, body)
	}

	// ===== ListBlobs shows uploaded blob (STATEFUL) =====

	body, status = azGet(t, base+"/mycontainer?restype=container&comp=list", sharedKey)
	if status != 200 {
		t.Fatalf("list blobs -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "EnumerationResults") {
		t.Fatalf("list blobs: missing EnumerationResults; body %s", body)
	}
	if !strings.Contains(body, "test.json") {
		t.Fatalf("list blobs: uploaded blob test.json not found; body %s", body)
	}
	if !strings.Contains(body, "<BlobType>BlockBlob") {
		t.Fatalf("list blobs: missing BlobType; body %s", body)
	}
	if !strings.Contains(body, "<ContentLength>") {
		t.Fatalf("list blobs: missing ContentLength; body %s", body)
	}

	// ===== GET blob returns content =====

	body, status = azGet(t, base+"/mycontainer/test.json", sharedKey)
	if status != 200 {
		t.Fatalf("get blob -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("get blob: content mismatch; body %s", body)
	}

	// ===== HEAD blob returns metadata =====

	resp := azHead(t, base+"/mycontainer/test.json", sharedKey)
	if resp.StatusCode != 200 {
		t.Fatalf("head blob -> status %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("head blob: missing ETag header")
	}
	if resp.Header.Get("x-ms-blob-type") == "" {
		t.Fatal("head blob: missing x-ms-blob-type header")
	}
	if resp.Header.Get("Content-Length") == "" {
		t.Fatal("head blob: missing Content-Length header")
	}
	if resp.Header.Get("x-ms-creation-time") == "" {
		t.Fatal("head blob: missing x-ms-creation-time header")
	}

	// ===== SAS token query form works =====

	sasURL := base + "/mycontainer/test.json?sv=2024-08-04&ss=b&srt=co&sp=r&sig=dHVudA==&se=2025-01-01T00:00:00Z"
	body, status = azGetNoAuth(t, sasURL)
	if status != 200 {
		t.Fatalf("SAS GET -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("SAS GET: content mismatch; body %s", body)
	}

	// ===== ListContainers (XML) =====

	body, status = azGet(t, base+"/?comp=list", sharedKey)
	if status != 200 {
		t.Fatalf("list containers -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "EnumerationResults") {
		t.Fatalf("list containers: missing EnumerationResults; body %s", body)
	}
	if !strings.Contains(body, "mycontainer") {
		t.Fatalf("list containers: missing mycontainer; body %s", body)
	}

	// ===== Set + Get blob metadata =====

	body, status = azPut(t, base+"/mycontainer/test.json?comp=metadata", sharedKey, nil)
	if status != 200 {
		t.Fatalf("set metadata -> status %d, want 200; body %s", status, body)
	}
	body, status = azGet(t, base+"/mycontainer/test.json?comp=metadata", sharedKey)
	if status != 200 {
		t.Fatalf("get metadata -> status %d, want 200; body %s", status, body)
	}

	// ===== DELETE blob → 202 =====

	resp = azDelete(t, base+"/mycontainer/test.json", sharedKey)
	if resp.StatusCode != 202 {
		t.Fatalf("delete blob -> status %d, want 202", resp.StatusCode)
	}

	// ===== GET deleted blob → 404 BlobNotFound =====

	body, status = azGet(t, base+"/mycontainer/test.json", sharedKey)
	if status != 404 {
		t.Fatalf("get deleted blob -> status %d, want 404; body %s", status, body)
	}
	if !strings.Contains(body, "BlobNotFound") {
		t.Fatalf("get deleted: missing BlobNotFound; body %s", body)
	}

	// ===== Without auth → 401 =====

	body, status = azGetNoAuth(t, base+"/?comp=list")
	if status != 401 {
		t.Fatalf("list containers without auth -> status %d, want 401; body %s", status, body)
	}
	if !strings.Contains(body, "NoAuthenticationInformation") {
		t.Fatalf("list without auth: missing NoAuthenticationInformation; body %s", body)
	}

	// ===== Bearer token works =====

	body, status = azGet(t, base+"/?comp=list", "Bearer eyJhbGciOiJIUzI1NiJ9.fake.token")
	if status != 200 {
		t.Fatalf("list containers with bearer -> status %d, want 200; body %s", status, body)
	}

	// ===== Malformed SharedKey → 403 =====

	body, status = azGet(t, base+"/?comp=list", "SharedKey acct")
	if status != 403 {
		t.Fatalf("malformed SharedKey -> status %d, want 403; body %s", status, body)
	}
	if !strings.Contains(body, "AuthenticationFailed") {
		t.Fatalf("malformed SharedKey: missing AuthenticationFailed; body %s", body)
	}

	// ===== Put blob to nonexistent container → 404 =====

	body, status = azPut(t, base+"/nonexistent/blob.txt", sharedKey, []byte("data"))
	if status != 404 {
		t.Fatalf("put to nonexistent container -> status %d, want 404; body %s", status, body)
	}
	if !strings.Contains(body, "ContainerNotFound") {
		t.Fatalf("put nonexistent: missing ContainerNotFound; body %s", body)
	}
}

// TestAzureStorageStyleBlockBlobLifecycle drives the real Azure block-blob
// staging protocol:
//
//   - Put Block (?comp=block&blockid=<base64>) stages bytes out of order,
//     each 201 carrying a Content-MD5 of the block bytes
//   - Get Block List before commit shows them as UncommittedBlocks
//   - Put Block List with a never-staged block id → 400 InvalidBlockList
//   - Put Block List (correct, listed order) commits and assembles the
//     blob — GET returns the assembled bytes byte-exact
//   - Get Block List after commit shows CommittedBlocks in commit order
//     and the staged blocks are consumed
//   - blocklisttype validation: missing → 400 MissingRequiredQueryParameter,
//     bogus value → 400 InvalidQueryParameterValue
//   - Put Block without blockid → 400 MissingRequiredQueryParameter
//   - Get Block List on an unknown blob → 404 BlobNotFound
//   - deleting the blob discards staged blocks
func TestAzureStorageStyleBlockBlobLifecycle(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "azure-storage-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"azure": {Adapter: adapterDir},
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
	base := addrs["azure"]

	const sharedKey = "SharedKey stuntstorage:dHVudA=="

	if _, status := azPut(t, base+"/blockcont", sharedKey, nil); status != 201 {
		t.Fatalf("create container -> %d", status)
	}

	// Three multi-KB blocks staged OUT OF ORDER (c, a, b) — the committed
	// order comes from the block LIST, not the staging order.
	blockA := azTestBytes(5*1024, 1)
	blockB := azTestBytes(4*1024, 2)
	blockC := azTestBytes(3*1024, 3)
	idA := base64.StdEncoding.EncodeToString([]byte("block-A"))
	idB := base64.StdEncoding.EncodeToString([]byte("block-B"))
	idC := base64.StdEncoding.EncodeToString([]byte("block-C"))
	blobURL := base + "/blockcont/report.bin"

	for _, tc := range []struct {
		id   string
		data []byte
	}{
		{idC, blockC}, {idA, blockA}, {idB, blockB},
	} {
		hdr, status := azPutHeaders(t, blobURL+"?comp=block&blockid="+url.QueryEscape(tc.id), sharedKey, tc.data)
		if status != 201 {
			t.Fatalf("put block %s -> %d", tc.id, status)
		}
		wantMD5 := base64.StdEncoding.EncodeToString(azSHA256(tc.data))
		if got := hdr.Get("Content-MD5"); got != wantMD5 {
			t.Fatalf("put block %s Content-MD5 = %q, want %q", tc.id, got, wantMD5)
		}
	}

	// Missing blockid → 400 MissingRequiredQueryParameter.
	if body, status := azPut(t, blobURL+"?comp=block", sharedKey, []byte("x")); status != 400 || !strings.Contains(body, "MissingRequiredQueryParameter") {
		t.Fatalf("put block without blockid -> %d %q, want 400 MissingRequiredQueryParameter", status, body)
	}

	// Before commit: uncommitted blocks listed, nothing committed.
	body, status := azGet(t, blobURL+"?comp=blocklist&blocklisttype=all", sharedKey)
	if status != 200 {
		t.Fatalf("get blocklist (uncommitted) -> %d; body %s", status, body)
	}
	for _, want := range []string{idA, idB, idC} {
		if !strings.Contains(body, "<Name>"+want+"</Name>") {
			t.Fatalf("blocklist missing staged block %s: %s", want, body)
		}
	}
	if strings.Contains(body, "<CommittedBlocks>") == false || strings.Count(body, "<Block>") != 3 {
		t.Fatalf("blocklist should show 3 uncommitted blocks only: %s", body)
	}

	// blocklisttype validation.
	if body, status := azGet(t, blobURL+"?comp=blocklist", sharedKey); status != 400 || !strings.Contains(body, "MissingRequiredQueryParameter") {
		t.Fatalf("blocklist without type -> %d %q, want 400 MissingRequiredQueryParameter", status, body)
	}
	if body, status := azGet(t, blobURL+"?comp=blocklist&blocklisttype=bogus", sharedKey); status != 400 || !strings.Contains(body, "InvalidQueryParameterValue") {
		t.Fatalf("blocklist bogus type -> %d %q, want 400 InvalidQueryParameterValue", status, body)
	}
	if body, status := azGet(t, base+"/blockcont/noblocks.bin?comp=blocklist&blocklisttype=all", sharedKey); status != 404 || !strings.Contains(body, "BlobNotFound") {
		t.Fatalf("blocklist unknown blob -> %d %q, want 404 BlobNotFound", status, body)
	}

	// Commit listing a never-staged block → 400 InvalidBlockList.
	neverStaged := base64.StdEncoding.EncodeToString([]byte("never-staged"))
	badList := azBlockListBody([]string{idA, neverStaged, idB})
	if body, status := azPut(t, blobURL+"?comp=blocklist", sharedKey, []byte(badList)); status != 400 || !strings.Contains(body, "InvalidBlockList") {
		t.Fatalf("blocklist with never-staged block -> %d %q, want 400 InvalidBlockList", status, body)
	}

	// Commit in the listed order (A, B, C — independent of staging order).
	goodList := azBlockListBody([]string{idA, idB, idC})
	hdr, status := azPutHeaders(t, blobURL+"?comp=blocklist", sharedKey, []byte(goodList))
	if status != 201 {
		t.Fatalf("put blocklist -> %d", status)
	}
	if hdr.Get("ETag") == "" || hdr.Get("Last-Modified") == "" {
		t.Fatalf("put blocklist missing ETag/Last-Modified: %v %v", hdr.Get("ETag"), hdr.Get("Last-Modified"))
	}

	// Assembled blob downloads byte-exact: blockA + blockB + blockC.
	body, status = azGet(t, blobURL, sharedKey)
	if status != 200 {
		t.Fatalf("get committed blob -> %d; body %s", status, body)
	}
	want := append(append(append([]byte{}, blockA...), blockB...), blockC...)
	if !bytes.Equal([]byte(body), want) {
		t.Fatalf("committed blob mismatch: got %d bytes, want %d", len(body), len(want))
	}

	// After commit: committed blocks in list order, staged blocks consumed.
	body, status = azGet(t, blobURL+"?comp=blocklist&blocklisttype=all", sharedKey)
	if status != 200 {
		t.Fatalf("get blocklist (committed) -> %d", status)
	}
	aIdx := strings.Index(body, "<Name>"+idA+"</Name>")
	bIdx := strings.Index(body, "<Name>"+idB+"</Name>")
	cIdx := strings.Index(body, "<Name>"+idC+"</Name>")
	if aIdx < 0 || bIdx < 0 || cIdx < 0 || !(aIdx < bIdx && bIdx < cIdx) {
		t.Fatalf("committed blocks not in commit order: %s", body)
	}
	if strings.Contains(body, "<UncommittedBlocks>\n    <Block>") || strings.Count(body, "<Block>") != 3 {
		t.Fatalf("staged blocks should be consumed by the commit: %s", body)
	}
	body, status = azGet(t, blobURL+"?comp=blocklist&blocklisttype=uncommitted", sharedKey)
	if status != 200 || strings.Contains(body, "<Block>") {
		t.Fatalf("uncommitted-only view should be empty: %d %s", status, body)
	}

	// Deleting the blob discards any freshly staged blocks.
	if _, st := azPutHeaders(t, blobURL+"?comp=block&blockid="+url.QueryEscape(idA), sharedKey, blockA); st != 201 {
		t.Fatalf("re-stage after commit -> %d", st)
	}
	resp := azDelete(t, blobURL, sharedKey)
	if resp.StatusCode != 202 {
		t.Fatalf("delete blob -> %d, want 202", resp.StatusCode)
	}
	if body, status := azGet(t, blobURL+"?comp=blocklist&blocklisttype=all", sharedKey); status != 404 || !strings.Contains(body, "BlobNotFound") {
		t.Fatalf("blocklist after delete -> %d %q, want 404 BlobNotFound (staged blocks discarded)", status, body)
	}
}

// azTestBytes returns n deterministic pseudo-random bytes for block/binary
// round-trip tests.
func azTestBytes(n int, seed int) []byte {
	out := make([]byte, n)
	x := uint32(seed)*2654435761 + 98765
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 17)
	}
	return out
}

// azSHA256 returns the raw SHA-256 digest of b.
func azSHA256(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// azBlockListBody builds a Put Block List XML body listing the block ids
// (as <Latest> entries, the common Azure SDK form).
func azBlockListBody(ids []string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, id := range ids {
		b.WriteString("<Latest>" + id + "</Latest>")
	}
	b.WriteString("</BlockList>")
	return b.String()
}

// azPutHeaders is azPut but also returns the response headers.
func azPutHeaders(t *testing.T, rawurl, auth string, body []byte) (http.Header, int) {
	t.Helper()
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	req.Header.Set("Authorization", azMaybeSign(req, auth))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.Header, resp.StatusCode
}

// === Azure Storage test helpers ===

// azSharedKey computes a real SharedKey Authorization header signed with the
// adapter's documented synthetic key (must match scripts/lib.star).
func azSharedKey(req *http.Request, account string) string {
	key, _ := base64.StdEncoding.DecodeString(base64.StdEncoding.EncodeToString([]byte("stunt-local-storage-signing-key")))
	cl := req.Header.Get("Content-Length")
	if cl == "0" {
		cl = ""
	}
	lines := []string{
		req.Method,
		req.Header.Get("Content-Encoding"),
		req.Header.Get("Content-Language"),
		cl,
		req.Header.Get("Content-MD5"),
		req.Header.Get("Content-Type"),
		req.Header.Get("Date"),
		req.Header.Get("If-Modified-Since"),
		req.Header.Get("If-Match"),
		req.Header.Get("If-None-Match"),
		req.Header.Get("If-Unmodified-Since"),
		req.Header.Get("Range"),
	}
	sts := strings.Join(lines, "\n") + "\n"
	var xms []string
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-ms-") {
			xms = append(xms, lk+":"+strings.TrimSpace(v[0]))
		}
	}
	sort.Strings(xms)
	for _, h := range xms {
		sts += h + "\n"
	}
	u := req.URL
	sts += "/" + account + u.Path
	var qk []string
	for k := range u.Query() {
		qk = append(qk, strings.ToLower(k))
	}
	sort.Strings(qk)
	for _, k := range qk {
		v := u.Query().Get(k)
		sts += "\n" + k + ":" + v
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(sts))
	return "SharedKey " + account + ":" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// azMaybeSign swaps the historical placeholder credential for a real
// SharedKey signature; other values (e.g. malformed tokens in negative
// tests) pass through verbatim.
func azMaybeSign(req *http.Request, auth string) string {
	if auth == "SharedKey stuntstorage:dHVudA==" {
		return azSharedKey(req, "stuntstorage")
	}
	return auth
}

func azGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", azMaybeSign(req, auth))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func azGetNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func azPut(t *testing.T, rawurl, auth string, body []byte) (string, int) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest("PUT", rawurl, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	// Sign last: the adapter signs every header the wire carries.
	req.Header.Set("Authorization", azMaybeSign(req, auth))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func azHead(t *testing.T, rawurl, auth string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("HEAD", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", azMaybeSign(req, auth))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func azDelete(t *testing.T, rawurl, auth string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("DELETE", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", azMaybeSign(req, auth))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
