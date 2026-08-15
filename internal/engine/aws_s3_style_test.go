package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	"stuntapi.com/stunt/internal/primitives/clock"
)

// Synthetic AWS credentials shared by the aws-s3-style and aws-iam-sts-style
// adapters (documented in their READMEs). Tests sign requests with real
// SigV4 (crypto/hmac) so the positive/negative paths exercise the adapters'
// signature recomputation, not a bypass.
const (
	awsStyleAccessKey = "AKIAIOSFODNN7EXAMPLE"
	awsStyleSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	awsStyleRegion    = "us-east-1"
	awsStyleBadSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYWRONGSECRET0"
)

func awsSHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func awsHMACSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// awsSigV4URIEncode percent-encodes s per RFC 3986 (the SigV4 rules). When
// keepSlash is true "/" stays literal (canonical URI); otherwise it is
// encoded (canonical query).
func awsSigV4URIEncode(s string, keepSlash bool) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 || (keepSlash && c == '/') {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// awsSigV4CanonicalQuery builds the canonical query string: keys sorted,
// keys and values RFC 3986-encoded, "k=v" joined with "&".
func awsSigV4CanonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, awsSigV4URIEncode(k, false)+"="+awsSigV4URIEncode(q.Get(k), false))
	}
	return strings.Join(parts, "&")
}

// awsSigV4SigningKey derives the SigV4 signing key:
// HMAC(HMAC(HMAC(HMAC("AWS4"+secret, date), region), service), "aws4_request").
func awsSigV4SigningKey(secret, date, region, service string) []byte {
	kDate := awsHMACSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := awsHMACSHA256(kDate, []byte(region))
	kService := awsHMACSHA256(kRegion, []byte(service))
	return awsHMACSHA256(kService, []byte("aws4_request"))
}

// awsSigV4Sign signs req in place with real SigV4 using the given
// credentials. For the s3 service the x-amz-content-sha256 header is set
// and signed (as real S3 SDKs do); for other services only host and
// x-amz-date are signed and the payload hash covers the body bytes.
func awsSigV4Sign(t *testing.T, req *http.Request, body []byte, service, accessKey, secretKey string, at time.Time) {
	t.Helper()
	amzDate := at.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	payloadHash := awsSHA256Hex(body)
	req.Header.Set("x-amz-date", amzDate)

	signedHeaders := []string{"host", "x-amz-date"}
	if service == "s3" {
		req.Header.Set("x-amz-content-sha256", payloadHash)
		signedHeaders = []string{"host", "x-amz-content-sha256", "x-amz-date"}
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	var ch strings.Builder
	for _, h := range signedHeaders {
		v := ""
		if h == "host" {
			v = host
		} else {
			v = req.Header.Get(h)
		}
		ch.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	path := req.URL.Path
	if path == "" {
		path = "/"
	}
	creq := req.Method + "\n" +
		awsSigV4URIEncode(path, true) + "\n" +
		awsSigV4CanonicalQuery(req.URL.Query()) + "\n" +
		ch.String() + "\n" +
		strings.Join(signedHeaders, ";") + "\n" +
		payloadHash

	scope := date + "/" + awsStyleRegion + "/" + service + "/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + awsSHA256Hex([]byte(creq))
	key := awsSigV4SigningKey(secretKey, date, awsStyleRegion, service)
	sig := hex.EncodeToString(awsHMACSHA256(key, []byte(sts)))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+
		", SignedHeaders="+strings.Join(signedHeaders, ";")+", Signature="+sig)
}

// awsSigV4Presign returns rawurl with a real SigV4 presigned GET query
// string (UNSIGNED-PAYLOAD, as S3 presigned GETs use).
func awsSigV4Presign(t *testing.T, rawurl string, expiresSeconds int64, at time.Time) string {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	amzDate := at.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	scope := date + "/" + awsStyleRegion + "/s3/aws4_request"
	q := u.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", awsStyleAccessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.FormatInt(expiresSeconds, 10))
	q.Set("X-Amz-SignedHeaders", "host")

	path := u.Path
	if path == "" {
		path = "/"
	}
	creq := "GET\n" +
		awsSigV4URIEncode(path, true) + "\n" +
		awsSigV4CanonicalQuery(q) + "\n" +
		"host:" + u.Host + "\n" + "\n" +
		"host\n" +
		"UNSIGNED-PAYLOAD"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + awsSHA256Hex([]byte(creq))
	key := awsSigV4SigningKey(awsStyleSecretKey, date, awsStyleRegion, "s3")
	sig := hex.EncodeToString(awsHMACSHA256(key, []byte(sts)))
	q.Set("X-Amz-Signature", sig)
	u.RawQuery = q.Encode()
	return u.String()
}

// awsTamperSignature flips the first hex digit of the Signature component
// of an already-signed Authorization header, keeping it well-formed.
func awsTamperSignature(auth string) string {
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

// TestAwsS3StyleAdapter exercises the Amazon S3-style adapter end-to-end:
//
//   - PUT bucket (create)
//   - PUT object with real SigV4 → 200 with content-derived ETag
//   - ListObjectsV2 (XML) shows the uploaded object (STATEFUL)
//   - GET object returns content
//   - HEAD object returns metadata headers (incl. RFC 1123 Last-Modified)
//   - DELETE object → 204
//   - GET deleted object → 404 NoSuchKey XML
//   - GET without auth → 403 MissingSecurityHeader XML
//   - NoSuchBucket XML error
//   - Presigned URL with a real SigV4 signature works
//   - Malformed auth → 403
//   - Location constraint
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
	now := time.Now()

	// ===== Create bucket =====

	body, status := s3Put(t, base+"/mybucket", nil, now)
	if status != 200 {
		t.Fatalf("create bucket -> status %d, want 200; body %s", status, body)
	}

	// ===== Upload object with real SigV4 =====

	uploadContent := `{"hello":"world"}`
	etag, status := s3PutETag(t, base+"/mybucket/test.txt", []byte(uploadContent), now)
	if status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}
	// ETag is content-derived: the quoted SHA-256 hex digest of the bytes.
	wantETag := `"` + awsSHA256Hex([]byte(uploadContent)) + `"`
	if etag != wantETag {
		t.Fatalf("put object ETag = %q, want %q", etag, wantETag)
	}

	// ===== ListObjectsV2 shows the uploaded object (STATEFUL) =====

	body, status = s3Get(t, base+"/mybucket?list-type=2", now)
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

	body, status = s3Get(t, base+"/mybucket/test.txt", now)
	if status != 200 {
		t.Fatalf("get object -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("get object: content mismatch; body %s", body)
	}

	// ===== HEAD object returns metadata (no body) =====

	resp := s3Head(t, base+"/mybucket/test.txt", now)
	if resp.StatusCode != 200 {
		t.Fatalf("head object -> status %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("ETag") != wantETag {
		t.Fatalf("head object ETag = %q, want %q", resp.Header.Get("ETag"), wantETag)
	}
	if resp.Header.Get("Content-Length") != strconv.Itoa(len(uploadContent)) {
		t.Fatalf("head object Content-Length = %q, want %d", resp.Header.Get("Content-Length"), len(uploadContent))
	}
	lm := resp.Header.Get("Last-Modified")
	if lm == "" {
		t.Fatal("head object: missing Last-Modified header")
	}
	if _, err := http.ParseTime(lm); err != nil {
		t.Fatalf("head object Last-Modified %q is not RFC 1123: %v", lm, err)
	}

	// ===== DELETE object → 204 =====

	resp = s3Delete(t, base+"/mybucket/test.txt", now)
	if resp.StatusCode != 204 {
		t.Fatalf("delete object -> status %d, want 204", resp.StatusCode)
	}

	// ===== GET deleted object → 404 NoSuchKey =====

	body, status = s3Get(t, base+"/mybucket/test.txt", now)
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

	body, status = s3Get(t, base+"/nonexistent-bucket?list-type=2", now)
	if status != 404 {
		t.Fatalf("list nonexistent bucket -> status %d, want 404; body %s", status, body)
	}
	if !strings.Contains(body, "NoSuchBucket") {
		t.Fatalf("list nonexistent: missing NoSuchBucket; body %s", body)
	}

	// ===== Presigned URL GET works (real SigV4 signature) =====

	// First upload a second object.
	if _, status := s3Put(t, base+"/mybucket/test2.txt", []byte(`{"data":"presigned"}`), now); status != 200 {
		t.Fatalf("put object (presigned prep) -> status %d, want 200", status)
	}

	presignedURL := awsSigV4Presign(t, base+"/mybucket/test2.txt", 300, now)
	body, status = s3GetNoAuth(t, presignedURL)
	if status != 200 {
		t.Fatalf("presigned GET -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "presigned") {
		t.Fatalf("presigned GET: content mismatch; body %s", body)
	}

	// ===== Malformed auth → 403 =====

	body, status = s3GetRawAuth(t, base+"/mybucket?list-type=2", "Bearer some-token")
	if status != 403 {
		t.Fatalf("list with bad auth -> status %d, want 403; body %s", status, body)
	}
	if !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("list with bad auth: missing SignatureDoesNotMatch; body %s", body)
	}

	// ===== Location constraint =====

	body, status = s3Get(t, base+"/mybucket?location", now)
	if status != 200 {
		t.Fatalf("location -> status %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, "LocationConstraint") {
		t.Fatalf("location: missing LocationConstraint; body %s", body)
	}
}

// TestAwsS3StyleSigV4Verification drives the adapter's real SigV4
// recomputation with a deterministic virtual clock: a correctly signed
// request passes, tampered signatures / wrong secrets / unknown access
// keys are rejected with real AWS error envelopes, stale x-amz-date hits
// the skew window, and presigned URLs expire with the clock.
func TestAwsS3StyleSigV4Verification(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "aws-s3-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	vc := clock.NewVirtualClock(time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC))
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"s3": {Adapter: adapterDir},
		},
	}
	e, err := New(m, WithClock(vc))
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
	now := vc.Now()

	if _, status := s3Put(t, base+"/vbucket", nil, now); status != 200 {
		t.Fatalf("create bucket -> %d", status)
	}
	if _, status := s3Put(t, base+"/vbucket/data.txt", []byte("sigv4-payload"), now); status != 200 {
		t.Fatalf("put object -> %d", status)
	}

	// Tampered signature → 403 SignatureDoesNotMatch.
	req := s3SignedReq(t, "GET", base+"/vbucket/data.txt", nil, now, awsStyleAccessKey, awsStyleSecretKey)
	req.Header.Set("Authorization", awsTamperSignature(req.Header.Get("Authorization")))
	body, status := s3Do(t, req)
	if status != 403 {
		t.Fatalf("tampered signature -> status %d, want 403; body %s", status, body)
	}
	if !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("tampered signature: missing SignatureDoesNotMatch; body %s", body)
	}

	// Wrong secret (right shape, right AKID) → 403 SignatureDoesNotMatch.
	req = s3SignedReq(t, "GET", base+"/vbucket/data.txt", nil, now, awsStyleAccessKey, awsStyleBadSecret)
	body, status = s3Do(t, req)
	if status != 403 || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("wrong secret -> status %d; body %s", status, body)
	}

	// Unknown access key ID → 403 InvalidAccessKeyId.
	req = s3SignedReq(t, "GET", base+"/vbucket/data.txt", nil, now, "AKIAUNKNOWNKEY000000", awsStyleSecretKey)
	body, status = s3Do(t, req)
	if status != 403 || !strings.Contains(body, "InvalidAccessKeyId") {
		t.Fatalf("unknown AKID -> status %d; body %s", status, body)
	}

	// Tampered payload: sign one body, send another → 400
	// XAmzContentSHA256Mismatch, like real S3 (the payload-hash header is
	// checked against the verbatim bytes).
	tampered := []byte("sigv4-payload-tampered")
	req = s3SignedReq(t, "PUT", base+"/vbucket/data.txt", []byte("sigv4-payload"), now, awsStyleAccessKey, awsStyleSecretKey)
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	req.ContentLength = int64(len(tampered))
	body, status = s3Do(t, req)
	if status != 400 || !strings.Contains(body, "XAmzContentSHA256Mismatch") {
		t.Fatalf("tampered payload -> status %d; body %s", status, body)
	}

	// Stale x-amz-date beyond the 15-minute window → 403 RequestTimeTooSkewed.
	vc.Advance(20 * time.Minute)
	body, status = s3Get(t, base+"/vbucket/data.txt", now) // signed at the old time
	if status != 403 || !strings.Contains(body, "RequestTimeTooSkewed") {
		t.Fatalf("stale request -> status %d; body %s", status, body)
	}

	// Presigned URL: valid for 60s from the original signing time — that
	// window has now elapsed, so it must be rejected as expired.
	presigned := awsSigV4Presign(t, base+"/vbucket/data.txt", 60, now)
	body, status = s3GetNoAuth(t, presigned)
	if status != 403 || !strings.Contains(body, "Request has expired") {
		t.Fatalf("expired presigned -> status %d; body %s", status, body)
	}

	// A fresh presigned URL still works at the advanced clock time.
	presigned = awsSigV4Presign(t, base+"/vbucket/data.txt", 300, vc.Now())
	body, status = s3GetNoAuth(t, presigned)
	if status != 200 || !strings.Contains(body, "sigv4-payload") {
		t.Fatalf("fresh presigned -> status %d; body %s", status, body)
	}

	// A fresh signed request works again after the clock moved on.
	body, status = s3Get(t, base+"/vbucket/data.txt", vc.Now())
	if status != 200 || !strings.Contains(body, "sigv4-payload") {
		t.Fatalf("fresh signed request -> status %d; body %s", status, body)
	}
}

// === S3 test helpers ===

// s3SignedReq builds a request and signs it with real SigV4 (s3 service,
// given credentials).
func s3SignedReq(t *testing.T, method, rawurl string, body []byte, at time.Time, accessKey, secretKey string) *http.Request {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawurl, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	awsSigV4Sign(t, req, body, "s3", accessKey, secretKey, at)
	return req
}

func s3Do(t *testing.T, req *http.Request) (string, int) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func s3Get(t *testing.T, rawurl string, at time.Time) (string, int) {
	t.Helper()
	return s3Do(t, s3SignedReq(t, "GET", rawurl, nil, at, awsStyleAccessKey, awsStyleSecretKey))
}

func s3GetRawAuth(t *testing.T, rawurl, auth string) (string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	return s3Do(t, req)
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

func s3Put(t *testing.T, rawurl string, body []byte, at time.Time) (string, int) {
	t.Helper()
	return s3Do(t, s3SignedReq(t, "PUT", rawurl, body, at, awsStyleAccessKey, awsStyleSecretKey))
}

// s3PutETag is s3Put but also returns the response ETag header.
func s3PutETag(t *testing.T, rawurl string, body []byte, at time.Time) (string, int) {
	t.Helper()
	req := s3SignedReq(t, "PUT", rawurl, body, at, awsStyleAccessKey, awsStyleSecretKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)
	return resp.Header.Get("ETag"), resp.StatusCode
}

func s3Head(t *testing.T, rawurl string, at time.Time) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(s3SignedReq(t, "HEAD", rawurl, nil, at, awsStyleAccessKey, awsStyleSecretKey))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func s3Delete(t *testing.T, rawurl string, at time.Time) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(s3SignedReq(t, "DELETE", rawurl, nil, at, awsStyleAccessKey, awsStyleSecretKey))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestAWSS3StyleBinaryRoundTrip proves binary content round-trips byte-exact:
// invalid-UTF-8 bytes (0xff/0xfe) that a JSON-backed collection would corrupt
// survive PUT then GET unchanged, with a correct Content-Length and a
// content-derived ETag.
func TestAWSS3StyleBinaryRoundTrip(t *testing.T) {
	adapterDir, err := filepath.Abs(filepath.Join("..", "..", "adapters", "aws-s3-style"))
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	m := &manifest.Manifest{
		Path:    filepath.Join(stateDir, "stunt.yaml"),
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: 0},
		Services: map[string]manifest.Service{
			"s3": {Adapter: adapterDir},
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
	now := time.Now()

	if _, status := s3Put(t, base+"/mybucket", nil, now); status != 200 {
		t.Fatalf("create bucket -> %d", status)
	}

	// PNG magic + invalid-UTF-8 bytes (0xff/0xfe) + NUL + valid multibyte.
	bin := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe, 0x00, 0x80, 0xc3, 0xa9}
	etag, status := s3PutETag(t, base+"/mybucket/bin.dat", bin, now)
	if status != 200 {
		t.Fatalf("put binary -> %d", status)
	}
	if etag != `"`+awsSHA256Hex(bin)+`"` {
		t.Fatalf("binary ETag = %q, want quoted sha256 of the bytes", etag)
	}

	got, status := s3Get(t, base+"/mybucket/bin.dat", now)
	if status != 200 {
		t.Fatalf("get binary -> %d", status)
	}
	if !bytes.Equal([]byte(got), bin) {
		t.Fatalf("binary round-trip mismatch: got %v (%d bytes), want %v (%d bytes)", []byte(got), len(got), bin, len(bin))
	}

	// HEAD reports the byte length, not a UTF-8-rounded one.
	hdr := s3Head(t, base+"/mybucket/bin.dat", now)
	if hdr.Header.Get("Content-Length") != strconv.Itoa(len(bin)) {
		t.Fatalf("HEAD Content-Length = %q, want %d", hdr.Header.Get("Content-Length"), len(bin))
	}
}
