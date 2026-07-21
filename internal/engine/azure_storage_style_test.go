package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
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

// === Azure Storage test helpers ===

func azGet(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
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
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-ms-blob-type", "BlockBlob")
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
	req.Header.Set("Authorization", auth)
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
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
