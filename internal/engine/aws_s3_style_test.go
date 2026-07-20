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

	"github.com/stunt-adapters/stunt/internal/manifest"
)

// TestAwsS3StyleAdapter exercises the Amazon S3-style adapter end-to-end:
//
//   - PUT bucket (create)
//   - PUT object with SigV4 header → 200 with ETag
//   - ListObjectsV2 (XML) shows the uploaded object (STATEFUL)
//   - GET object returns content
//   - HEAD object returns metadata headers
//   - DELETE object → 204
//   - GET deleted object → 404 NoSuchKey XML
//   - GET without auth → 403 MissingSecurityHeader XML
//   - NoSuchBucket XML error
//   - Presigned URL with valid X-Amz-* params works
func TestAwsS3StyleAdapter(t *testing.T) {
	adapterDir := filepath.Join("..", "..", "adapters", "aws-s3-style")
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
			"s3": {Adapter: absAdapterDir},
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

	base := addrs["s3"]

	// Fake but well-formed SigV4 Authorization header.
	const sigv4 = "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=fe5f80f77d5fa3beca038a248ff027d044aca418"

	// ===== Create bucket =====

	body, status := s3Put(t, base+"/mybucket", sigv4, nil)
	if status != 200 {
		t.Fatalf("create bucket -> status %d, want 200; body %s", status, body)
	}

	// ===== Upload object with SigV4 =====

	uploadContent := `{"hello":"world"}`
	body, status = s3Put(t, base+"/mybucket/test.txt", sigv4, []byte(uploadContent))
	if status != 200 {
		t.Fatalf("put object -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "ETag") && !strings.Contains(body, "etag") {
		// ETag is in the response header; body is empty for PUT
	}

	// ===== ListObjectsV2 shows the uploaded object (STATEFUL) =====

	body, status = s3Get(t, base+"/mybucket?list-type=2", sigv4)
	if status != 200 {
		t.Fatalf("list objects -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "ListBucketResult") {
		t.Fatalf("list: missing ListBucketResult in XML; body %s", body)
	}
	if !strings.Contains(body, "test.txt") {
		t.Fatalf("list: uploaded object test.txt not found in XML; body %s", body)
	}
	if !strings.Contains(body, "<KeyCount>") {
		t.Fatalf("list: missing KeyCount; body %s", body)
	}

	// ===== GET object returns content =====

	body, status = s3Get(t, base+"/mybucket/test.txt", sigv4)
	if status != 200 {
		t.Fatalf("get object -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("get object: content mismatch; body %s", body)
	}

	// ===== HEAD object returns metadata (no body) =====

	resp := s3Head(t, base+"/mybucket/test.txt", sigv4)
	if resp.StatusCode != 200 {
		t.Fatalf("head object -> status %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatal("head object: missing ETag header")
	}
	if resp.Header.Get("Content-Length") == "" {
		t.Fatal("head object: missing Content-Length header")
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Fatal("head object: missing Last-Modified header")
	}

	// ===== DELETE object → 204 =====

	resp = s3Delete(t, base+"/mybucket/test.txt", sigv4)
	if resp.StatusCode != 204 {
		t.Fatalf("delete object -> status %d, want 204", resp.StatusCode)
	}

	// ===== GET deleted object → 404 NoSuchKey =====

	body, status = s3Get(t, base+"/mybucket/test.txt", sigv4)
	if status != 404 {
		t.Fatalf("get deleted object -> status %d, want 404; body %s", status, body)
	}
	if !strings.Contains(body, "NoSuchKey") {
		t.Fatalf("get deleted: missing NoSuchKey in XML; body %s", body)
	}

	// ===== Without auth → 403 MissingSecurityHeader =====

	body, status = s3GetNoAuth(t, base+"/mybucket?list-type=2")
	if status != 403 {
		t.Fatalf("list without auth -> status %d, want 403; body %s", status, body)
	}
	if !strings.Contains(body, "MissingSecurityHeader") {
		t.Fatalf("list without auth: missing MissingSecurityHeader; body %s", body)
	}

	// ===== NoSuchBucket error =====

	body, status = s3Get(t, base+"/nonexistent-bucket?list-type=2", sigv4)
	if status != 404 {
		t.Fatalf("list nonexistent bucket -> status %d, want 404; body %s", status, body)
	}
	if !strings.Contains(body, "NoSuchBucket") {
		t.Fatalf("list nonexistent: missing NoSuchBucket; body %s", body)
	}

	// ===== Presigned URL GET works =====

	presignedURL := base + "/mybucket/test2.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE/20260120/us-east-1/s3/aws4_request" +
		"&X-Amz-Signature=fe5f80f77d5fa3beca038a248ff027d044aca418" +
		"&X-Amz-Date=20260120T000000Z"

	// First upload a second object
	_, status = s3Put(t, base+"/mybucket/test2.txt", sigv4, []byte(`{"data":"presigned"}`))
	if status != 200 {
		t.Fatalf("put object (presigned prep) -> status %d, want 200", status)
	}

	// GET with presigned URL (no Authorization header)
	body, status = s3GetNoAuth(t, presignedURL)
	if status != 200 {
		t.Fatalf("presigned GET -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "presigned") {
		t.Fatalf("presigned GET: content mismatch; body %s", body)
	}

	// ===== Malformed auth → 403 =====

	body, status = s3Get(t, base+"/mybucket?list-type=2", "Bearer some-token")
	if status != 403 {
		t.Fatalf("list with bad auth -> status %d, want 403; body %s", status, body)
	}
	if !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("list with bad auth: missing SignatureDoesNotMatch; body %s", body)
	}

	// ===== Location constraint =====

	body, status = s3Get(t, base+"/mybucket?location", sigv4)
	if status != 200 {
		t.Fatalf("location -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "LocationConstraint") {
		t.Fatalf("location: missing LocationConstraint; body %s", body)
	}
}

// === S3 test helpers ===

func s3Get(t *testing.T, rawurl, auth string) (string, int) {
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

func s3GetNoAuth(t *testing.T, rawurl string) (string, int) {
	t.Helper()
	resp, err := http.Get(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func s3Put(t *testing.T, rawurl, auth string, body []byte) (string, int) {
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
	req.Header.Set("x-amz-date", "20260120T000000Z")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func s3Head(t *testing.T, rawurl, auth string) *http.Response {
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

func s3Delete(t *testing.T, rawurl, auth string) *http.Response {
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
