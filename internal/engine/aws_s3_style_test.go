package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
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

func awsMD5Hex(b []byte) string {
	sum := md5.Sum(b)
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
	awsSigV4SignPayload(t, req, awsSHA256Hex(body), service, accessKey, secretKey, at)
}

// awsSigV4SignPayload signs req in place with real SigV4 using an explicit
// payload hash. Streaming (aws-chunked) uploads sign the STREAMING literal
// rather than the body bytes, so chunked tests pass the literal here.
func awsSigV4SignPayload(t *testing.T, req *http.Request, payloadHash, service, accessKey, secretKey string, at time.Time) {
	amzDate := at.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
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
	// ETag is content-derived: the quoted MD5 hex digest of the bytes.
	wantETag := `"` + awsMD5Hex([]byte(uploadContent)) + `"`
	if etag != wantETag {
		t.Fatalf("put object ETag = %q, want %q", etag, wantETag)
	}

	// md5("hello") = 5d41402abc4b2a76b9719d911017c592 (S3 compat checksum).
	helloETag, status := s3PutETag(t, base+"/mybucket/hello.txt", []byte("hello"), now)
	if status != 200 {
		t.Fatalf("put hello -> status %d, want 200", status)
	}
	if helloETag != `"5d41402abc4b2a76b9719d911017c592"` {
		t.Fatalf("put hello ETag = %q, want %q", helloETag, `"5d41402abc4b2a76b9719d911017c592"`)
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

// TestAwsS3StyleMultipartUpload drives the full S3 multipart upload
// protocol with real SigV4-signed requests:
//
//   - POST ?uploads → 200 InitiateMultipartUploadResult with an UploadId
//   - UploadPart (out of order: 3, then 1, then 2) → per-part ETags that
//     equal the quoted MD5 of the part bytes
//   - ListParts → parts in ascending order with max-parts /
//     part-number-marker paging
//   - CompleteMultipartUpload with a missing part → 400 InvalidPart
//   - CompleteMultipartUpload with a non-ascending list → 400 InvalidPartOrder
//   - CompleteMultipartUpload (correct) → 200 with the multipart ETag
//     ("...-3"), and the assembled object round-trips byte-exact via GET
//   - AbortMultipartUpload → 204 and the object stays absent (404 NoSuchKey),
//     with every later part/complete/list call 404 NoSuchUpload
//   - partNumber=0 → 400 InvalidArgument
func TestAwsS3StyleMultipartUpload(t *testing.T) {
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

	if _, status := s3Put(t, base+"/mpubucket", nil, now); status != 200 {
		t.Fatalf("create bucket -> %d", status)
	}

	// Multi-KB binary payload split into three unequal parts (5 KiB + 4 KiB
	// + 3 KiB = 12 KiB). Parts 3 and 1 are swapped below to prove out-of-order
	// acceptance.
	part1 := s3TestBytes(5*1024, 1)
	part2 := s3TestBytes(4*1024, 2)
	part3 := s3TestBytes(3*1024, 3)
	full := append(append(append([]byte{}, part1...), part2...), part3...)

	// ===== Create multipart upload =====
	body, status := s3Post(t, base+"/mpubucket/multi.bin?uploads", nil, now)
	if status != 200 {
		t.Fatalf("create mpu -> %d; body %s", status, body)
	}
	uploadID := s3XMLTag(t, body, "UploadId")
	if uploadID == "" {
		t.Fatalf("create mpu: no UploadId in %s", body)
	}
	partURL := base + "/mpubucket/multi.bin?uploadId=" + uploadID

	// ===== UploadPart, deliberately out of order (3, then 1, then 2) =====
	etags := map[int]string{}
	for _, tc := range []struct {
		n    int
		data []byte
	}{
		{3, part3}, {1, part1}, {2, part2},
	} {
		etag, st := s3PutETag(t, fmt.Sprintf("%s&partNumber=%d", partURL, tc.n), tc.data, now)
		if st != 200 {
			t.Fatalf("upload part %d -> %d", tc.n, st)
		}
		want := `"` + awsMD5Hex(tc.data) + `"`
		if etag != want {
			t.Fatalf("upload part %d ETag = %q, want %q", tc.n, etag, want)
		}
		etags[tc.n] = strings.Trim(etag, `"`)
	}

	// partNumber out of range → 400 InvalidArgument.
	body, status = s3Put(t, partURL+"&partNumber=0", []byte("x"), now)
	if status != 400 || !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("partNumber=0 -> %d %q, want 400 InvalidArgument", status, body)
	}

	// Unknown upload id → 404 NoSuchUpload.
	body, status = s3Put(t, base+"/mpubucket/multi.bin?partNumber=1&uploadId=mpu_nope", []byte("x"), now)
	if status != 404 || !strings.Contains(body, "NoSuchUpload") {
		t.Fatalf("part to unknown upload -> %d %q, want 404 NoSuchUpload", status, body)
	}

	// ===== ListParts: ascending order + paging =====
	body, status = s3Get(t, partURL+"&max-parts=2", now)
	if status != 200 {
		t.Fatalf("list parts -> %d; body %s", status, body)
	}
	if !strings.Contains(body, "<PartNumber>1</PartNumber>") || !strings.Contains(body, "<PartNumber>2</PartNumber>") {
		t.Fatalf("list parts page 1 missing parts 1,2: %s", body)
	}
	if strings.Contains(body, "<PartNumber>3</PartNumber>") {
		t.Fatalf("list parts page 1 should not contain part 3: %s", body)
	}
	if !strings.Contains(body, "<IsTruncated>true</IsTruncated>") || !strings.Contains(body, "<NextPartNumberMarker>2</NextPartNumberMarker>") {
		t.Fatalf("list parts page 1 paging elements wrong: %s", body)
	}
	body, status = s3Get(t, partURL+"&max-parts=2&part-number-marker=2", now)
	if status != 200 || !strings.Contains(body, "<PartNumber>3</PartNumber>") || strings.Contains(body, "<IsTruncated>true") {
		t.Fatalf("list parts page 2 wrong: %d %s", status, body)
	}

	// ===== Complete: missing part → 400 InvalidPart =====
	missingBody := s3CompleteBody([][2]string{{"1", etags[1]}, {"2", etags[2]}, {"4", etags[3]}})
	body, status = s3Post(t, partURL, []byte(missingBody), now)
	if status != 400 || !strings.Contains(body, "InvalidPart") {
		t.Fatalf("complete with missing part -> %d %q, want 400 InvalidPart", status, body)
	}

	// ===== Complete: non-ascending list → 400 InvalidPartOrder =====
	outOfOrder := s3CompleteBody([][2]string{{"3", etags[3]}, {"1", etags[1]}, {"2", etags[2]}})
	body, status = s3Post(t, partURL, []byte(outOfOrder), now)
	if status != 400 || !strings.Contains(body, "InvalidPartOrder") {
		t.Fatalf("complete out of order -> %d %q, want 400 InvalidPartOrder", status, body)
	}

	// ===== Complete: wrong part ETag → 400 InvalidPart =====
	wrongEtag := s3CompleteBody([][2]string{{"1", etags[1]}, {"2", strings.Repeat("0", 32)}, {"3", etags[3]}})
	body, status = s3Post(t, partURL, []byte(wrongEtag), now)
	if status != 400 || !strings.Contains(body, "InvalidPart") {
		t.Fatalf("complete with wrong etag -> %d %q, want 400 InvalidPart", status, body)
	}

	// ===== Complete: correct → 200, assembled object byte-exact =====
	completeBody := s3CompleteBody([][2]string{{"1", etags[1]}, {"2", etags[2]}, {"3", etags[3]}})
	body, status = s3Post(t, partURL, []byte(completeBody), now)
	if status != 200 {
		t.Fatalf("complete -> %d; body %s", status, body)
	}
	// The composite ETag is MD5 over the concatenated BINARY part digests,
	// suffixed with the part count. Asserted exactly: a suffix-only check lets
	// any 32-hex-plus-"-3" pass, so a digest composed from the wrong bytes — the
	// part bodies instead of the part digests, say — would sail through.
	etag := strings.ReplaceAll(s3XMLTag(t, body, "ETag"), "&quot;", "")
	h := md5.New()
	for _, n := range []int{1, 2, 3} {
		raw, err := hex.DecodeString(etags[n])
		if err != nil {
			t.Fatalf("part %d etag %q is not hex: %v", n, etags[n], err)
		}
		h.Write(raw)
	}
	wantETag := hex.EncodeToString(h.Sum(nil)) + "-3"
	if etag != wantETag {
		t.Fatalf("complete ETag = %q, want %q (md5 of the concatenated part digests)", etag, wantETag)
	}

	got, status := s3Get(t, base+"/mpubucket/multi.bin", now)
	if status != 200 {
		t.Fatalf("get assembled object -> %d", status)
	}
	if !bytes.Equal([]byte(got), full) {
		t.Fatalf("assembled object mismatch: got %d bytes, want %d", len(got), len(full))
	}
	hdr := s3Head(t, base+"/mpubucket/multi.bin", now)
	if hdr.Header.Get("Content-Length") != strconv.Itoa(len(full)) {
		t.Fatalf("assembled HEAD Content-Length = %q, want %d", hdr.Header.Get("Content-Length"), len(full))
	}
	if hdr.Header.Get("ETag") != `"`+strings.Trim(etag, `"`)+`"` {
		t.Fatalf("assembled HEAD ETag = %q, want %q", hdr.Header.Get("ETag"), etag)
	}

	// The upload is gone after completion.
	body, status = s3Get(t, partURL, now)
	if status != 404 || !strings.Contains(body, "NoSuchUpload") {
		t.Fatalf("list parts after complete -> %d %q, want 404 NoSuchUpload", status, body)
	}

	// ===== Abort discards the upload and leaves the object absent =====
	body, status = s3Post(t, base+"/mpubucket/aborted.bin?uploads", nil, now)
	if status != 200 {
		t.Fatalf("create mpu (abort case) -> %d", status)
	}
	uploadID2 := s3XMLTag(t, body, "UploadId")
	abortURL := base + "/mpubucket/aborted.bin?uploadId=" + uploadID2
	if _, st := s3PutETag(t, abortURL+"&partNumber=1", part1, now); st != 200 {
		t.Fatalf("upload part (abort case) -> %d", st)
	}
	resp := s3Delete(t, abortURL, now)
	if resp.StatusCode != 204 {
		t.Fatalf("abort -> %d, want 204", resp.StatusCode)
	}
	if body, status = s3Get(t, base+"/mpubucket/aborted.bin", now); status != 404 || !strings.Contains(body, "NoSuchKey") {
		t.Fatalf("get aborted object -> %d %q, want 404 NoSuchKey", status, body)
	}
	if body, status = s3Get(t, abortURL, now); status != 404 || !strings.Contains(body, "NoSuchUpload") {
		t.Fatalf("list parts after abort -> %d %q, want 404 NoSuchUpload", status, body)
	}
	// Completing an aborted upload fails closed.
	if body, status = s3Post(t, abortURL, []byte(s3CompleteBody([][2]string{{"1", etags[1]}})), now); status != 404 || !strings.Contains(body, "NoSuchUpload") {
		t.Fatalf("complete after abort -> %d %q, want 404 NoSuchUpload", status, body)
	}
}

// s3TestBytes returns n deterministic pseudo-random bytes seeded by tag
// (multi-KB payloads with invalid-UTF-8 patterns for the round-trip tests).
func s3TestBytes(n int, seed int) []byte {
	out := make([]byte, n)
	x := uint32(seed)*2654435761 + 12345
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 13)
	}
	return out
}

// s3XMLTag extracts the first <tag>…</tag> text from an S3 XML body.
func s3XMLTag(t *testing.T, body, tag string) string {
	t.Helper()
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	i += len(open)
	j := strings.Index(body[i:], close)
	if j < 0 {
		return ""
	}
	return body[i : i+j]
}

// s3CompleteBody builds a CompleteMultipartUpload request body from ordered
// (part number, etag) pairs.
func s3CompleteBody(parts [][2]string) string {
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		b.WriteString("<Part><PartNumber>" + p[0] + "</PartNumber><ETag>\"" + p[1] + "\"</ETag></Part>")
	}
	b.WriteString("</CompleteMultipartUpload>")
	return b.String()
}

// s3Post issues a SigV4-signed POST with an optional body.
func s3Post(t *testing.T, rawurl string, body []byte, at time.Time) (string, int) {
	t.Helper()
	return s3Do(t, s3SignedReq(t, "POST", rawurl, body, at, awsStyleAccessKey, awsStyleSecretKey))
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
	if etag != `"`+awsMD5Hex(bin)+`"` {
		t.Fatalf("binary ETag = %q, want quoted md5 of the bytes", etag)
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

// TestAWSS3Conditionals exercises S3 conditional preconditions:
// PUT/DELETE ETag-only, GET/HEAD full with timestamps, 412/304 codes.
func TestAWSS3Conditionals(t *testing.T) {
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

	doCond := func(method, rawurl string, body []byte, cond map[string]string) (string, int, http.Header) {
		t.Helper()
		req := s3SignedReq(t, method, rawurl, body, now, awsStyleAccessKey, awsStyleSecretKey)
		for k, v := range cond {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.StatusCode, resp.Header
	}

	if _, status := s3Put(t, base+"/condbucket", nil, now); status != 200 {
		t.Fatalf("create bucket -> %d", status)
	}

	// The ETag is the validator these preconditions are compared against, and
	// the conditionals below only mean something if it is the real content
	// digest, so assert it exactly rather than checking its shape.
	etag, status := s3PutETag(t, base+"/condbucket/file.txt", []byte("hello"), now)
	if status != 200 {
		t.Fatalf("put file -> %d", status)
	}
	if want := `"` + awsMD5Hex([]byte("hello")) + `"`; etag != want {
		t.Fatalf("put file ETag = %q, want %q", etag, want)
	}
	wrongETag := `"00000000000000000000000000000000"`

	lastModStr := vc.Now().UTC().Format(http.TimeFormat)
	futureStr := vc.Now().Add(48 * time.Hour).UTC().Format(http.TimeFormat)
	pastStr := vc.Now().Add(-48 * time.Hour).UTC().Format(http.TimeFormat)
	// single-digit future day + UTC/UT variants + TAB OWS + month-ci
	singleDigitFuture := vc.Now().Add(15 * 24 * time.Hour).UTC().Format("Mon, 2 Jan 2006 15:04:05 GMT")
	_ = singleDigitFuture
	utcStr := vc.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 UTC")
	utStr := vc.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 UT")
	tabStr := "Tue,\t20 Jan 2026 12:00:00 GMT"
	multiSPStr := "Tue,  20  Jan  2026  12:00:00  GMT"
	monthCIStr := "Tue, 20 JAN 2026 12:00:00 GMT"
	weekdayIgnoreFuture := "Frobnicate, " + vc.Now().Add(48*time.Hour).UTC().Format("02 Jan 2006 15:04:05 GMT")
	feb30Str := "Mon, 30 Feb 2026 00:00:00 GMT"
	feb29NonLeap := "Sun, 29 Feb 2026 12:00:00 GMT"
	feb29LeapFuture := "Sat, 29 Feb 2028 12:00:00 GMT"

	// PUT If-None-Match:* on existing -> 412
	if body, st, _ := doCond("PUT", base+"/condbucket/file.txt", []byte("hello"), map[string]string{"If-None-Match": "*"}); st != 412 {
		t.Fatalf("put conflict INM:* -> status %d, want 412; body %s", st, body)
	}
	// PUT If-Match wrong -> 412
	if body, st, _ := doCond("PUT", base+"/condbucket/file.txt", []byte("hello"), map[string]string{"If-Match": wrongETag}); st != 412 {
		t.Fatalf("put If-Match wrong -> status %d, want 412; body %s", st, body)
	}
	// PUT missing + If-Match:* -> 412
	if body, st, _ := doCond("PUT", base+"/condbucket/missing.txt", []byte("x"), map[string]string{"If-Match": "*"}); st != 412 {
		t.Fatalf("put missing If-Match:* -> status %d, want 412; body %s", st, body)
	}
	// PUT missing + INM:* -> 200 (create allowed)
	if body, st, _ := doCond("PUT", base+"/condbucket/created.txt", []byte("x"), map[string]string{"If-None-Match": "*"}); st != 200 {
		t.Fatalf("put missing INM:* -> status %d, want 200; body %s", st, body)
	}
	// DELETE missing no-cond -> 204
	if _, st, _ := doCond("DELETE", base+"/condbucket/nokey.txt", nil, nil); st != 204 {
		t.Fatalf("delete missing no-cond -> status %d, want 204", st)
	}
	// DELETE missing + If-Match -> 412
	if body, st, _ := doCond("DELETE", base+"/condbucket/nokey2.txt", nil, map[string]string{"If-Match": "*"}); st != 412 {
		t.Fatalf("delete missing If-Match -> status %d, want 412; body %s", st, body)
	}
	// DELETE existing wrong If-Match -> 412 (object must survive)
	if body, st, _ := doCond("DELETE", base+"/condbucket/file.txt", nil, map[string]string{"If-Match": wrongETag}); st != 412 {
		t.Fatalf("delete If-Match wrong -> status %d, want 412; body %s", st, body)
	}
	// GET If-None-Match:etag -> 304 empty + ETag/LM/req-id, no CT
	body, st, hdr := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-None-Match": etag})
	if st != 304 {
		t.Fatalf("get INM etag -> status %d, want 304; body %s", st, body)
	}
	if body != "" {
		t.Fatalf("get 304 body = %q, want empty", body)
	}
	if hdr.Get("ETag") != etag {
		t.Fatalf("get 304 ETag = %q, want %q", hdr.Get("ETag"), etag)
	}
	if hdr.Get("Last-Modified") == "" {
		t.Fatal("get 304 missing Last-Modified")
	}
	if hdr.Get("x-amz-request-id") == "" {
		t.Fatal("get 304 missing x-amz-request-id")
	}
	if hdr.Get("Content-Type") != "" {
		t.Fatalf("get 304 Content-Type = %q, want empty", hdr.Get("Content-Type"))
	}
	// GET If-None-Match:* on existing -> 304
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-None-Match": "*"}); st != 304 {
		t.Fatalf("get INM:* -> status %d, want 304", st)
	}
	// GET If-Match wrong -> 412 PreconditionFailed
	body, st, hdr = doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Match": wrongETag})
	if st != 412 {
		t.Fatalf("get If-Match wrong -> status %d, want 412; body %s", st, body)
	}
	if !strings.Contains(body, "PreconditionFailed") {
		t.Fatalf("get 412 missing PreconditionFailed; body %s", body)
	}
	if !strings.Contains(body, "<Condition>If-Match</Condition>") {
		t.Fatalf("get 412 missing Condition If-Match; body %s", body)
	}
	if hdr.Get("Content-Type") != "application/xml" {
		t.Fatalf("get 412 CT = %q, want application/xml", hdr.Get("Content-Type"))
	}
	if hdr.Get("x-amz-request-id") == "" {
		t.Fatal("get 412 missing x-amz-request-id")
	}
	// GET missing + If-Match -> 404 (not 412)
	if body, st, _ := doCond("GET", base+"/condbucket/nokey3.txt", nil, map[string]string{"If-Match": "*"}); st != 404 {
		t.Fatalf("get missing If-Match -> status %d, want 404; body %s", st, body)
	}
	// malformed ETag ignored -> 200
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Match": "W/"}); st != 200 {
		t.Fatalf("get malformed If-Match W/ -> status %d, want 200; body %s", st, body)
	}
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": "not-a-date"}); st != 200 {
		t.Fatalf("get malformed IMS -> status %d, want 200; body %s", st, body)
	}
	// combined If-Match fail + INM match -> 412 wins (not 304)
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Match": wrongETag, "If-None-Match": etag}); st != 412 {
		t.Fatalf("get combined If-Match-fail+INM -> status %d, want 412; body %s", st, body)
	}
	// *-in-list match-any
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-None-Match": `*, "abc"`}); st != 304 {
		t.Fatalf("get INM *-in-list -> status %d, want 304", st)
	}
	// quoted-empty no-match -> 200
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-None-Match": `""`}); st != 200 {
		t.Fatalf("get INM quoted-empty -> status %d, want 200; body %s", st, body)
	}
	// W/ weak -> strong match -> 200 for If-Match (pass)
	weak := `W/` + etag
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Match": weak}); st != 200 {
		t.Fatalf("get If-Match W/etag -> status %d, want 200; body %s", st, body)
	}
	// IMS equal -> 304, past -> 200, future -> 304
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": lastModStr}); st != 304 {
		t.Fatalf("get IMS equal -> status %d, want 304", st)
	}
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": pastStr}); st != 200 {
		t.Fatalf("get IMS past -> status %d, want 200; body %s", st, body)
	}
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": futureStr}); st != 304 {
		t.Fatalf("get IMS future -> status %d, want 304", st)
	}
	// IUS past -> 412, future -> 200, equal -> 200
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Unmodified-Since": pastStr}); st != 412 {
		t.Fatalf("get IUS past -> status %d, want 412; body %s", st, body)
	}
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Unmodified-Since": futureStr}); st != 200 {
		t.Fatalf("get IUS future -> status %d, want 200; body %s", st, body)
	}
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Unmodified-Since": lastModStr}); st != 200 {
		t.Fatalf("get IUS equal -> status %d, want 200; body %s", st, body)
	}
	// future/weekday-ignore/Feb30-ignore/legacy
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": weekdayIgnoreFuture}); st != 304 {
		t.Fatalf("get IMS weekday-ignore future -> status %d, want 304", st)
	}
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": feb30Str}); st != 200 {
		t.Fatalf("get IMS Feb30 -> status %d, want 200; body %s", st, body)
	}
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": feb29NonLeap}); st != 200 {
		t.Fatalf("get IMS Feb29-nonleap -> status %d, want 200; body %s", st, body)
	}
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": feb29LeapFuture}); st != 304 {
		t.Fatalf("get IMS Feb29-leap-future -> status %d, want 304", st)
	}
	if body, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": "0"}); st != 200 {
		t.Fatalf("get IMS legacy 0 -> status %d, want 200; body %s", st, body)
	}
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": utcStr}); st != 304 {
		t.Fatalf("get IMS UTC -> status %d, want 304", st)
	}
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": utStr}); st != 304 {
		t.Fatalf("get IMS UT -> status %d, want 304", st)
	}
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": tabStr}); st != 304 {
		t.Fatalf("get IMS TAB -> status %d, want 304", st)
	}
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": multiSPStr}); st != 304 {
		t.Fatalf("get IMS multi-SP -> status %d, want 304", st)
	}
	if _, st, _ := doCond("GET", base+"/condbucket/file.txt", nil, map[string]string{"If-Modified-Since": monthCIStr}); st != 304 {
		t.Fatalf("get IMS month-ci -> status %d, want 304", st)
	}
	// PUT timestamps ignored (ETags-only)
	if body, st, _ := doCond("PUT", base+"/condbucket/file.txt", []byte("hello"), map[string]string{"If-Unmodified-Since": pastStr}); st != 200 {
		t.Fatalf("put IUS past ignored -> status %d, want 200; body %s", st, body)
	}
	// HEAD mirrors GET
	if _, st, _ := doCond("HEAD", base+"/condbucket/file.txt", nil, map[string]string{"If-None-Match": etag}); st != 304 {
		t.Fatalf("head INM etag -> status %d, want 304", st)
	}
	if body, st, _ := doCond("HEAD", base+"/condbucket/file.txt", nil, map[string]string{"If-Match": wrongETag}); st != 412 {
		t.Fatalf("head If-Match wrong -> status %d, want 412; body %s", st, body)
	}
	// non-object ignore: List with If-Match wrong still 200
	if body, st, _ := doCond("GET", base+"/condbucket?list-type=2", nil, map[string]string{"If-Match": wrongETag}); st != 200 {
		t.Fatalf("list with If-Match -> status %d, want 200; body %s", st, body)
	}
}

func TestAWSS3Metadata(t *testing.T) {
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

	doReq := func(method, rawurl string, body []byte, hdrs map[string]string) (string, int, http.Header) {
		t.Helper()
		req := s3SignedReq(t, method, rawurl, body, now, awsStyleAccessKey, awsStyleSecretKey)
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.StatusCode, resp.Header
	}
	hasMeta := func(hdr http.Header) bool {
		for k := range hdr {
			if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") {
				return true
			}
		}
		return false
	}

	if _, status := s3Put(t, base+"/metabucket", nil, now); status != 200 {
		t.Fatalf("create bucket -> %d", status)
	}

	// PUT with user meta (mixed-case suffix, interior spaces preserved).
	meta := map[string]string{
		"X-Amz-Meta-Kind":  "sample",
		"X-Amz-Meta-Mixed": "VaLue",
		"X-Amz-Meta-Sp":    "a  b",
	}
	if body, st, _ := doReq("PUT", base+"/metabucket/file.txt", []byte("hello"), meta); st != 200 {
		t.Fatalf("put with meta -> status %d, want 200; body %s", st, body)
	}

	// GET echoes on 200 (suffix lowercased, values verbatim).
	body, st, hdr := doReq("GET", base+"/metabucket/file.txt", nil, nil)
	if st != 200 || body != "hello" {
		t.Fatalf("get -> status %d body %q, want 200 hello", st, body)
	}
	if got := hdr.Get("x-amz-meta-kind"); got != "sample" {
		t.Fatalf("get x-amz-meta-kind = %q, want %q (customMetadataPreserved:false)", got, "sample")
	}
	if got := hdr.Get("x-amz-meta-mixed"); got != "VaLue" {
		t.Fatalf("get x-amz-meta-mixed = %q, want %q", got, "VaLue")
	}
	if got := hdr.Get("x-amz-meta-sp"); got != "a  b" {
		t.Fatalf("get x-amz-meta-sp = %q, want interior spaces preserved", got)
	}

	// HEAD echoes too.
	_, st, hdr = doReq("HEAD", base+"/metabucket/file.txt", nil, nil)
	if st != 200 {
		t.Fatalf("head -> status %d, want 200", st)
	}
	if got := hdr.Get("x-amz-meta-kind"); got != "sample" {
		t.Fatalf("head x-amz-meta-kind = %q, want %q", got, "sample")
	}

	// First-wins: duplicate wire headers collapse to the first value
	// (engine headerMap v[0]; Starlark keeps the first suffix on collision).
	req := s3SignedReq(t, "PUT", base+"/metabucket/dup.txt", []byte("hello"), now, awsStyleAccessKey, awsStyleSecretKey)
	req.Header.Add("x-amz-meta-kind", "first")
	req.Header.Add("x-amz-meta-kind", "second")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("put dup meta -> status %d, want 200", resp.StatusCode)
	}
	_, _, hdr = doReq("GET", base+"/metabucket/dup.txt", nil, nil)
	if got := hdr.Get("x-amz-meta-kind"); got != "first" {
		t.Fatalf("get dup x-amz-meta-kind = %q, want first-wins %q", got, "first")
	}

	// Empty value is allowed (header present, value empty).
	if body, st, _ := doReq("PUT", base+"/metabucket/empty.txt", []byte("hello"), map[string]string{"x-amz-meta-empty": ""}); st != 200 {
		t.Fatalf("put empty-value meta -> status %d, want 200; body %s", st, body)
	}
	_, st, hdr = doReq("GET", base+"/metabucket/empty.txt", nil, nil)
	if st != 200 {
		t.Fatalf("get empty-value -> status %d, want 200", st)
	}
	if _, ok := hdr["X-Amz-Meta-Empty"]; !ok {
		t.Fatal("get empty-value: x-amz-meta-empty header missing, want present-but-empty")
	}
	if got := hdr.Get("x-amz-meta-empty"); got != "" {
		t.Fatalf("get empty-value = %q, want %q", got, "")
	}

	// Empty suffix (wire name "x-amz-meta-") -> 400 InvalidArgument.
	if body, st, _ := doReq("PUT", base+"/metabucket/bad.txt", []byte("hello"), map[string]string{"x-amz-meta-": "v"}); st != 400 || !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("put empty-suffix meta -> %d %q, want 400 InvalidArgument", st, body)
	}

	// Size cap: sum(len(suffix)+len(value)) over 2048 -> 400 MetadataTooLarge
	// (prefix excluded, post-dedup byte length; 2048 passes, 2049 fails).
	if body, st, _ := doReq("PUT", base+"/metabucket/ok2048.txt", []byte("hello"), map[string]string{"x-amz-meta-k": strings.Repeat("a", 2047)}); st != 200 {
		t.Fatalf("put 2048-total meta -> status %d, want 200; body %s", st, body)
	}
	if body, st, _ := doReq("PUT", base+"/metabucket/big.txt", []byte("hello"), map[string]string{"x-amz-meta-k": strings.Repeat("a", 2048)}); st != 400 || !strings.Contains(body, "MetadataTooLarge") {
		t.Fatalf("put 2049-total meta -> %d %q, want 400 MetadataTooLarge", st, body)
	}
	multi := map[string]string{"x-amz-meta-a": strings.Repeat("a", 1024), "x-amz-meta-b": strings.Repeat("b", 1024)}
	if body, st, _ := doReq("PUT", base+"/metabucket/multi-big.txt", []byte("hello"), multi); st != 400 || !strings.Contains(body, "MetadataTooLarge") {
		t.Fatalf("put multi-header oversize meta -> %d %q, want 400 MetadataTooLarge", st, body)
	}

	// Overwrite without meta clears (PUT replaces, not merges).
	if _, st, _ := doReq("PUT", base+"/metabucket/file.txt", []byte("hello"), nil); st != 200 {
		t.Fatalf("overwrite no-meta -> status %d, want 200", st)
	}
	_, st, hdr = doReq("GET", base+"/metabucket/file.txt", nil, nil)
	if st != 200 {
		t.Fatalf("get after overwrite -> status %d, want 200", st)
	}
	if hasMeta(hdr) {
		t.Fatalf("get after overwrite carries meta headers, want cleared: %v", hdr)
	}
	// Objects stored without meta carry none (legacy {} fallback: no crash).
	_, st, hdr = doReq("HEAD", base+"/metabucket/file.txt", nil, nil)
	if st != 200 || hasMeta(hdr) {
		t.Fatalf("head after overwrite -> status %d meta %v, want 200 no-meta", st, hdr)
	}

	// Re-arm meta for the no-meta-on-error paths below.
	if body, st, _ := doReq("PUT", base+"/metabucket/file.txt", []byte("hello"), map[string]string{"x-amz-meta-kind": "sample"}); st != 200 {
		t.Fatalf("re-put with meta -> status %d; body %s", st, body)
	}
	_, st, hdr = doReq("GET", base+"/metabucket/file.txt", nil, nil)
	if hdr.Get("x-amz-meta-kind") != "sample" {
		t.Fatalf("re-put meta echo = %q, want sample", hdr.Get("x-amz-meta-kind"))
	}

	// List carries no user meta in response headers.
	_, st, hdr = doReq("GET", base+"/metabucket?list-type=2", nil, nil)
	if st != 200 {
		t.Fatalf("list -> status %d, want 200", st)
	}
	if hasMeta(hdr) {
		t.Fatalf("list carries meta headers, want none: %v", hdr)
	}

	// MPU: Create meta propagates through Complete; UploadPart meta and
	// Complete-request meta are ignored (no 400, not stored).
	body, st, _ = doReq("POST", base+"/metabucket/mpu.bin?uploads", nil, map[string]string{"x-amz-meta-kind": "mpuval"})
	if st != 200 {
		t.Fatalf("mpu create with meta -> status %d; body %s", st, body)
	}
	uploadID := s3XMLTag(t, body, "UploadId")
	if uploadID == "" {
		t.Fatalf("mpu create: no UploadId in %s", body)
	}
	partURL := base + "/metabucket/mpu.bin?uploadId=" + uploadID
	if body, st, _ := doReq("PUT", partURL+"&partNumber=1", []byte("hello"), map[string]string{"x-amz-meta-evil": "yes"}); st != 200 {
		t.Fatalf("upload part with meta -> status %d, want 200 ignored; body %s", st, body)
	}
	// The ETag digest algorithm is covered separately; CompleteMultipartUpload
	// matches on whatever the UploadPart response returned.
	partETag := awsMD5Hex([]byte("hello"))
	completeBody := s3CompleteBody([][2]string{{"1", partETag}})
	if body, st, _ := doReq("POST", partURL, []byte(completeBody), map[string]string{"x-amz-meta-other": "yes"}); st != 200 {
		t.Fatalf("complete with meta -> status %d, want 200 ignored; body %s", st, body)
	}
	_, st, hdr = doReq("GET", base+"/metabucket/mpu.bin", nil, nil)
	if st != 200 {
		t.Fatalf("get mpu object -> status %d, want 200", st)
	}
	if got := hdr.Get("x-amz-meta-kind"); got != "mpuval" {
		t.Fatalf("get mpu x-amz-meta-kind = %q, want %q (Create→Complete propagation)", got, "mpuval")
	}
	if hasMetaKey(hdr, "x-amz-meta-evil") || hasMetaKey(hdr, "x-amz-meta-other") {
		t.Fatalf("get mpu carries ignored part/complete meta, want only create meta: %v", hdr)
	}

	// MPU Create validates too: oversize -> 400 MetadataTooLarge, empty
	// suffix -> 400 InvalidArgument; no upload is created.
	if body, st, _ := doReq("POST", base+"/metabucket/mpu-big.bin?uploads", nil, map[string]string{"x-amz-meta-k": strings.Repeat("a", 2048)}); st != 400 || !strings.Contains(body, "MetadataTooLarge") {
		t.Fatalf("mpu create oversize meta -> %d %q, want 400 MetadataTooLarge", st, body)
	}
	if body, st, _ := doReq("POST", base+"/metabucket/mpu-bad.bin?uploads", nil, map[string]string{"x-amz-meta-": "v"}); st != 400 || !strings.Contains(body, "InvalidArgument") {
		t.Fatalf("mpu create empty-suffix meta -> %d %q, want 400 InvalidArgument", st, body)
	}
}

func hasMetaKey(hdr http.Header, key string) bool {
	for k := range hdr {
		if strings.ToLower(k) == key {
			return true
		}
	}
	return false
}

// TestAWSS3BucketDeleteBlockedByInFlightUpload pins the real S3 rule that a
// bucket with an in-progress multipart upload cannot be deleted. Without it
// the upload row and its part blobs survive the delete, leaving an upload
// that is listable but can never be aborted and has no ListMultipartUploads
// route to discover it by.
func TestAWSS3BucketDeleteBlockedByInFlightUpload(t *testing.T) {
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

	if _, status := s3Put(t, base+"/mpublock", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	// A second bucket stays deletable while the first has an upload pending:
	// the guard is scoped by bucket name, not "any upload exists".
	if _, status := s3Put(t, base+"/mpuother", nil, now); status != 200 {
		t.Fatalf("create second bucket -> status %d, want 200", status)
	}

	// An initiated upload with no parts uploaded is already in progress, so
	// it blocks the delete on its own.
	body, status := s3Post(t, base+"/mpublock/multi.bin?uploads", nil, now)
	if status != 200 {
		t.Fatalf("create upload -> status %d, want 200; body %s", status, body)
	}
	uploadID := s3XMLTag(t, body, "UploadId")
	if uploadID == "" {
		t.Fatalf("no UploadId in %s", body)
	}
	partURL := base + "/mpublock/multi.bin?uploadId=" + uploadID

	resp := s3Delete(t, base+"/mpublock", now)
	delBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("delete bucket with in-flight upload -> status %d, want 409; body %s", resp.StatusCode, delBody)
	}
	if !strings.Contains(string(delBody), "BucketNotEmpty") {
		t.Fatalf("delete bucket -> body %s, want BucketNotEmpty", delBody)
	}

	// A second part upload after the refused delete still works, proving the
	// upload was left intact rather than half-torn-down.
	partBody, status := s3Put(t, partURL+"&partNumber=1", []byte("hello"), now)
	if status != 200 {
		t.Fatalf("upload part after refused delete -> status %d, want 200; body %s", status, partBody)
	}
	if body, status := s3Get(t, partURL, now); status != 200 || !strings.Contains(body, "Part") {
		t.Fatalf("list parts after refused delete -> status %d; body %s", status, body)
	}

	// Still blocked once a part is staged.
	resp = s3Delete(t, base+"/mpublock", now)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("delete bucket with staged part -> status %d, want 409", resp.StatusCode)
	}

	// ?uploads is not implemented, so it must fail loudly rather than fall
	// through to ListObjectsV2: a real SDK parses <ListBucketResult> as
	// "nothing pending" and would conclude the bucket is clear.
	body, status = s3Get(t, base+"/mpublock?uploads", now)
	if status != 501 {
		t.Fatalf("list multipart uploads -> status %d, want 501; body %s", status, body)
	}
	if strings.Contains(body, "ListBucketResult") {
		t.Fatalf("list multipart uploads fell through to ListObjectsV2: %s", body)
	}

	// The other bucket was never blocked, before or after the upload existed.
	resp = s3Delete(t, base+"/mpuother", now)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete unrelated bucket -> status %d, want 204", resp.StatusCode)
	}

	// Abort releases the bucket.
	resp = s3Delete(t, partURL, now)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("abort upload -> status %d, want 204", resp.StatusCode)
	}
	resp = s3Delete(t, base+"/mpublock", now)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete bucket after abort -> status %d, want 204", resp.StatusCode)
	}
	// The bucket row is really gone, not just refused again.
	if body, status := s3Get(t, base+"/mpublock?location", now); status != 404 {
		t.Fatalf("get location after delete -> status %d, want 404; body %s", status, body)
	}
}

// TestAWSS3BucketDeleteRefusedUntilEmpty covers the objects path through 409
// BucketNotEmpty, which had no coverage at all, and is a coverage test rather
// than a regression test: it passes on the pre-fix commit, where the delete
// checked only the objects collection. It became load-bearing when DeleteBucket
// started scanning mpu_uploads too — the final leg holds only if completing an
// upload removes its row instead of leaving the bucket blocked forever.
// TestAWSS3BucketDeleteBlockedByInFlightUpload is the regression test.
func TestAWSS3BucketDeleteRefusedUntilEmpty(t *testing.T) {
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

	// A plain object blocks the delete on the objects path.
	if _, status := s3Put(t, base+"/busy", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if _, status := s3Put(t, base+"/busy/a.txt", []byte("hello"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}
	resp := s3Delete(t, base+"/busy", now)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 409 || !strings.Contains(string(body), "BucketNotEmpty") {
		t.Fatalf("delete bucket with object -> status %d, want 409 BucketNotEmpty; body %s", resp.StatusCode, body)
	}

	// Completing an upload clears its upload row; only the resulting object
	// should still be holding the delete.
	if body, status := s3Post(t, base+"/busy/multi.bin?uploads", nil, now); status != 200 {
		t.Fatalf("create upload -> status %d, want 200; body %s", status, body)
	} else {
		uploadID := s3XMLTag(t, body, "UploadId")
		partURL := base + "/busy/multi.bin?uploadId=" + uploadID
		rawETag, partStatus := s3PutETag(t, partURL+"&partNumber=1", []byte("hello"), now)
		if partStatus != 200 {
			t.Fatalf("upload part -> status %d, want 200", partStatus)
		}
		partETag := strings.Trim(rawETag, `"`)
		if body, status := s3Post(t, partURL, []byte(s3CompleteBody([][2]string{{"1", partETag}})), now); status != 200 {
			t.Fatalf("complete upload -> status %d, want 200; body %s", status, body)
		}
	}
	resp = s3Delete(t, base+"/busy", now)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("delete bucket with two objects -> status %d, want 409", resp.StatusCode)
	}

	// Emptying the bucket releases the delete, which only holds if the
	// completed upload's row was actually removed.
	for _, key := range []string{"a.txt", "multi.bin"} {
		resp = s3Delete(t, base+"/busy/"+key, now)
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("delete object %s -> status %d, want 204", key, resp.StatusCode)
		}
	}
	resp = s3Delete(t, base+"/busy", now)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete empty bucket -> status %d, want 204", resp.StatusCode)
	}
}

// TestAWSS3ListMultipartUploadsNotImplemented pins that ?uploads answers 501
// rather than falling through to ListObjectsV2. DeleteBucket refuses with 409
// while an upload is in progress, so this endpoint is the only route a client
// has to discover which upload is blocking it. Until it is implemented, the
// escape hatch is the upload id, or `stunt clean` — documented in the README
// and CHANGELOG.
func TestAWSS3ListMultipartUploadsNotImplemented(t *testing.T) {
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

	if _, status := s3Put(t, base+"/lmu", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if body, status := s3Post(t, base+"/lmu/multi.bin?uploads", nil, now); status != 200 {
		t.Fatalf("create upload -> status %d, want 200; body %s", status, body)
	}
	// The bucket holds one object so a 200 ListBucketResult would be non-empty.
	if _, status := s3Put(t, base+"/lmu/a.txt", []byte("hello"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	body, status := s3Get(t, base+"/lmu?uploads", now)
	if status != 501 {
		t.Fatalf("GET ?uploads -> status %d, want 501; body %s", status, body)
	}
	if strings.Contains(body, "ListBucketResult") {
		t.Fatalf("GET ?uploads fell through to ListObjectsV2: %s", body)
	}
	if !strings.Contains(body, "ListMultipartUploads") {
		t.Fatalf("GET ?uploads -> body %s, want it to name the operation", body)
	}
}

// TestAWSS3UnimplementedSubresourcesNotListed pins that every bucket
// subresource this simulator lacks answers 501 NotImplemented instead of
// falling through to ListObjectsV2. A <ListBucketResult> body reads to an SDK
// as a successful empty result, so an unimplemented subresource must fail
// loudly — ?versions is the worst case, where a client would conclude a
// versioned bucket holds no versions.
func TestAWSS3UnimplementedSubresourcesNotListed(t *testing.T) {
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

	if _, status := s3Put(t, base+"/subs", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	// One object, so a 200 ListBucketResult would be non-empty and obvious.
	if _, status := s3Put(t, base+"/subs/a.txt", []byte("hello"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	for _, sub := range awsS3BucketSubresources() {
		if sub == "location" || sub == "uploads" {
			continue
		}
		body, status := s3Get(t, base+"/subs?"+sub, now)
		if status != 501 || !strings.Contains(body, "NotImplemented") {
			t.Fatalf("GET ?%s -> status %d, want 501 NotImplemented; body %s", sub, status, body)
		}
		if strings.Contains(body, "ListBucketResult") {
			t.Fatalf("GET ?%s fell through to ListObjectsV2: %s", sub, body)
		}
		// S3 subresource tokens are not case-sensitive, and a presigned or
		// hand-rolled client can send any casing. An exact-match guard would
		// return the bogus 200 ListBucketResult for these.
		for _, variant := range []string{strings.ToUpper(sub), strings.ToUpper(sub[:1]) + sub[1:]} {
			body, status := s3Get(t, base+"/subs?"+variant, now)
			if status != 501 {
				t.Fatalf("GET ?%s (case variant) -> status %d, want 501; body %s", variant, status, body)
			}
		}
	}

	// ?location and ?uploads are the two bucket tokens the adapter handles
	// itself; every other SDK token must answer 501.
	if body, status := s3Get(t, base+"/subs?location", now); status != 200 || !strings.Contains(body, "LocationConstraint") {
		t.Fatalf("GET ?location -> status %d; body %s", status, body)
	}
	if body, status := s3Get(t, base+"/subs?list-type=2", now); status != 200 || !strings.Contains(body, "ListBucketResult") {
		t.Fatalf("GET ?list-type=2 -> status %d; body %s", status, body)
	}
	// A modern SDK's own listing discriminator is not a bucket subresource.
	if body, status := s3Get(t, base+"/subs?list-type=2&x-id=ListObjectsV2", now); status != 200 {
		t.Fatalf("GET with x-id -> status %d, want 200; body %s", status, body)
	}
}

// TestAWSS3UnimplementedSubresourceEscapesXML pins XML escaping on the 501
// path. _xml_error interpolates its resource slot raw, so a bucket name
// carrying XML metacharacters would otherwise reflect them into the response
// document. Bucket names are not otherwise validated, so this is reachable
// with no special setup.
func TestAWSS3UnimplementedSubresourceEscapesXML(t *testing.T) {
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

	// Every XML predefined entity, as it appears after path decoding.
	const metachars = `a<b&c"d'e`
	if _, status := s3Put(t, base+"/"+metachars, nil, now); status != 200 {
		t.Fatalf("create bucket with metacharacters -> status %d, want 200", status)
	}

	for _, probe := range []string{"?versions", "?uploads"} {
		body, status := s3Get(t, base+"/"+metachars+probe, now)
		if status != 501 {
			t.Fatalf("GET %s -> status %d, want 501; body %s", probe, status, body)
		}
		for _, raw := range []string{"<Resource>a<b", "a&c", `c"d`, "d'e"} {
			if strings.Contains(body, raw) {
				t.Fatalf("GET %s reflected raw metacharacters %q: %s", probe, raw, body)
			}
		}
		if !strings.Contains(body, "&lt;b&amp;c&quot;d&#39;e") {
			t.Fatalf("GET %s -> body %s, want escaped metacharacters", probe, body)
		}
		// The document must still parse.
		if err := xml.Unmarshal([]byte(body), new(struct {
			XMLName xml.Name
		})); err != nil {
			t.Fatalf("GET %s -> body is not well-formed XML: %v; body %s", probe, err, body)
		}
	}
}

// awsS3BucketSubresources is the set of bucket-level `?subresource` tokens the
// pinned aws-sdk-go-v2 can emit, taken from the `httpbinding.SplitURI("/?…")`
// literals in its serializers. The S3 test list is expected to be derived from
// a source other than the adapter's own denylist, so a token added to the SDK
// but missed by the adapter fails here rather than silently falling through to
// ListObjectsV2.

func awsS3BucketSubresources() []string {
	return []string{
		"abac", "accelerate", "acl", "analytics", "cors", "delete", "encryption",
		"inventory", "lifecycle", "location", "logging", "metadataAnnotationTable",
		"metadataConfiguration", "metadataInventoryTable", "metadataJournalTable",
		"intelligent-tiering", "metadataTable", "metrics", "notification",
		"object-lock", "ownershipControls", "policy", "policyStatus",
		"publicAccessBlock", "replication", "requestPayment", "session",
		"tagging", "uploads", "versioning", "versions", "website",
	}
}

// TestAWSS3SubresourcesDoNotTouchTheBucket pins that an unimplemented
// subresource is rejected on every method that routes to /{bucket}, not just
// GET. The guard originally lived only in the ListObjectsV2 path, so
// `PUT /{bucket}?versioning` created a bucket and `DELETE /{bucket}?tagging`
// deleted one — real DeleteBucketTagging leaves the bucket in place.
func TestAWSS3SubresourcesDoNotTouchTheBucket(t *testing.T) {
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

	if _, status := s3Put(t, base+"/guard", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if _, status := s3Put(t, base+"/guard/keep.txt", []byte("hello"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	// DELETE on a subresource must not delete the bucket. This runs against an
	// EMPTY bucket on purpose: a non-empty one answers 409 BucketNotEmpty, which
	// would mask the destruction this is here to catch.
	if _, status := s3Put(t, base+"/emptyguard", nil, now); status != 200 {
		t.Fatalf("create empty bucket -> status %d, want 200", status)
	}
	for _, sub := range []string{"tagging", "lifecycle", "cors", "policy", "versioning", "acl", "website"} {
		resp := s3Delete(t, base+"/emptyguard?"+sub, now)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 501 || !strings.Contains(string(body), "NotImplemented") {
			t.Fatalf("DELETE ?%s -> status %d, want 501 NotImplemented; body %s", sub, resp.StatusCode, body)
		}
		if body, status := s3Get(t, base+"/emptyguard?location", now); status != 200 {
			t.Fatalf("DELETE ?%s destroyed the bucket -> status %d; body %s", sub, status, body)
		}
	}

	// The object must still be there too.
	if body, status := s3Get(t, base+"/guard/keep.txt", now); status != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("object lost after subresource DELETEs -> status %d; body %s", status, body)
	}

	// PUT on a subresource must not create a bucket.
	for _, sub := range []string{"versioning", "tagging", "policy", "acl"} {
		if body, status := s3Put(t, base+"/ghost?"+sub, nil, now); status != 501 {
			t.Fatalf("PUT ?%s -> status %d, want 501; body %s", sub, status, body)
		}
		if body, status := s3Get(t, base+"/ghost?location", now); status != 404 {
			t.Fatalf("PUT ?%s created a bucket -> status %d, want 404; body %s", sub, status, body)
		}
	}

	// POST ?delete is S3's multi-object delete, unimplemented here.
	body, status := s3Post(t, base+"/guard?delete", []byte("<Delete/>"), now)
	if status != 501 || !strings.Contains(body, "NotImplemented") {
		t.Fatalf("POST ?delete -> status %d, want 501 NotImplemented; body %s", status, body)
	}

	// A real create/delete still works.
	if _, status := s3Put(t, base+"/guard", nil, now); status != 409 {
		t.Fatalf("create existing bucket -> status %d, want 409", status)
	}
	resp := s3Delete(t, base+"/guard/keep.txt", now)
	resp.Body.Close()
	resp = s3Delete(t, base+"/guard", now)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete bucket -> status %d, want 204", resp.StatusCode)
	}
}

// TestAWSS3ObjectSubresourcesDoNotTouchTheObject pins that object-level
// ?subresource calls never fall through to PutObject/GetObject/
// HeadObject/DeleteObject. Real S3 selects an object subresource with a query
// parameter on the same routes, so without a guard `DELETE
// /{bucket}/{key}?tagging` deletes the object and `PUT
// /{bucket}/{key}?tagging` overwrites its content.
func TestAWSS3ObjectSubresourcesDoNotTouchTheObject(t *testing.T) {
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

	if _, status := s3Put(t, base+"/osub", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if _, status := s3Put(t, base+"/osub/keep.txt", []byte("hello"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	for _, sub := range awsS3ObjectSubresources() {
		resp := s3Delete(t, base+"/osub/keep.txt?"+sub, now)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 501 || !strings.Contains(string(body), "NotImplemented") {
			t.Fatalf("DELETE ?%s -> status %d, want 501 NotImplemented; body %s", sub, resp.StatusCode, body)
		}
		if body, status := s3Get(t, base+"/osub/keep.txt", now); status != 200 || !strings.Contains(body, "hello") {
			t.Fatalf("DELETE ?%s destroyed the object -> status %d; body %s", sub, status, body)
		}
	}

	// PUT on a subresource must not overwrite the content.
	for _, sub := range awsS3ObjectSubresources() {
		if body, status := s3Put(t, base+"/osub/keep.txt?"+sub, []byte("<Tagging/>"), now); status != 501 {
			t.Fatalf("PUT ?%s -> status %d, want 501; body %s", sub, status, body)
		}
		if body, status := s3Get(t, base+"/osub/keep.txt", now); status != 200 || !strings.Contains(body, "hello") {
			t.Fatalf("PUT ?%s overwrote the object -> status %d; body %s", sub, status, body)
		}
		// And must not create a new key either.
		if body, status := s3Put(t, base+"/osub/ghost.txt?"+sub, []byte("x"), now); status != 501 {
			t.Fatalf("PUT ?%s on a missing key -> status %d, want 501; body %s", sub, status, body)
		}
		if body, status := s3Get(t, base+"/osub/ghost.txt", now); status != 404 {
			t.Fatalf("PUT ?%s created the key -> status %d, want 404; body %s", sub, status, body)
		}
	}

	// GET/HEAD on a subresource must not read the object.
	for _, sub := range awsS3ObjectSubresources() {
		body, status := s3Get(t, base+"/osub/keep.txt?"+sub, now)
		if status != 501 {
			t.Fatalf("GET ?%s -> status %d, want 501; body %s", sub, status, body)
		}
		if strings.Contains(body, "hello") {
			t.Fatalf("GET ?%s returned the object body: %s", sub, body)
		}
		resp := s3Head(t, base+"/osub/keep.txt?"+sub, now)
		hbody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 501 {
			t.Fatalf("HEAD ?%s -> status %d, want 501; body %s", sub, resp.StatusCode, hbody)
		}
	}

	// The multipart routes and the plain object routes still work.
	body, status := s3Post(t, base+"/osub/multi.bin?uploads", nil, now)
	if status != 200 {
		t.Fatalf("initiate upload -> status %d, want 200; body %s", status, body)
	}
	uploadID := s3XMLTag(t, body, "UploadId")
	rawETag, partStatus := s3PutETag(t, base+"/osub/multi.bin?uploadId="+uploadID+"&partNumber=1", []byte("hello"), now)
	if partStatus != 200 {
		t.Fatalf("upload part -> status %d, want 200", partStatus)
	}
	if body, status := s3Post(t, base+"/osub/multi.bin?uploadId="+uploadID, []byte(s3CompleteBody([][2]string{{"1", strings.Trim(rawETag, `"`)}})), now); status != 200 {
		t.Fatalf("complete upload -> status %d, want 200; body %s", status, body)
	}
	if body, status := s3Get(t, base+"/osub/keep.txt", now); status != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("plain object round-trip -> status %d; body %s", status, body)
	}
}

// awsS3ObjectSubresources is the set of object-level `?subresource` tokens the
// pinned aws-sdk-go-v2 can emit, from its `/{Key+}?…` SplitURI literals. Derived
// independently of the adapter's denylist so a token the SDK can send but the
// adapter omits fails here rather than falling through to PutObject/
// DeleteObject.
func awsS3ObjectSubresources() []string {
	return []string{
		"acl", "annotation", "attributes", "encryption", "legal-hold",
		"renameObject", "restore", "retention", "select", "tagging", "torrent",
	}
}

// TestAWSS3ErrorMessageEscapesQueryKey pins that a query parameter's key is
// escaped before it is echoed into an error document. The bucket subresource
// guards name the caller's own query key, so a key carrying XML metacharacters
// must not be able to inject an element, close the <Message> element early, or
// produce a document no parser accepts.
func TestAWSS3ErrorMessageEscapesQueryKey(t *testing.T) {
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

	// Percent-encoded so the raw metacharacters reach the handler.
	probes := []string{
		"a%3Cb%26c=1",        // a<b&c=1
		"%22x%22=1",          // "x"=1
		"tagging%3Cx%3E=1",   // tagging<x>=1
		"%3C%2FError%3E=1",   // </Error>=1
		"%3C%2FMessage%3E=1", // </Message>=1
		"%27%27%3Cb%3E=1",    // ''<>=1
	}
	for _, probe := range probes {
		body, status := s3Put(t, base+"/msginj?"+probe, nil, now)
		if status != 501 {
			t.Fatalf("PUT ?%s -> status %d, want 501; body %s", probe, status, body)
		}
		if err := xml.Unmarshal([]byte(body), new(struct{ XMLName xml.Name })); err != nil {
			t.Fatalf("PUT ?%s -> response is not well-formed XML: %v; body %q", probe, err, body)
		}
		// The envelope legitimately ends with </Message> and </Error>, so the
		// detector for an injected element or a prematurely closed element is
		// the parse above. Here we only assert the metacharacters themselves
		// never appear raw.
		for _, raw := range []string{"a<b", "a&c", "<x>", "''<", "'<"} {
			if strings.Contains(body, raw) {
				t.Fatalf("PUT ?%s reflected %q unescaped: %q", probe, raw, body)
			}
		}

		// DELETE /{bucket} takes the same guard and must escape identically.
		resp := s3Delete(t, base+"/msginj?"+probe, now)
		delBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 501 {
			t.Fatalf("DELETE ?%s -> status %d, want 501; body %s", probe, resp.StatusCode, delBody)
		}
		if err := xml.Unmarshal([]byte(delBody), new(struct{ XMLName xml.Name })); err != nil {
			t.Fatalf("DELETE ?%s -> response is not well-formed XML: %v; body %q", probe, err, delBody)
		}
	}

	// A plain subresource still produces the normal, readable message.
	body, status := s3Put(t, base+"/msginj?versioning", nil, now)
	if status != 501 || !strings.Contains(body, "versioning is not implemented") {
		t.Fatalf("plain subresource -> status %d, body %s", status, body)
	}
}

// TestAWSS3ObjectWriteRoutesAreFailClosed pins that the mutating object routes
// allowlist rather than denylists query parameters. `?tagging` on PUT and DELETE
// is covered elsewhere; this pins the forward-comproperty: a token this
// simulator has never heard of must be refused, not treated as a plain
// PutObject/DeleteObject. The bucket routes have the same property.
func TestAWSS3ObjectWriteRoutesAreFailClosed(t *testing.T) {
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

	if _, status := s3Put(t, base+"/fc", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if _, status := s3Put(t, base+"/fc/keep.txt", []byte("hello"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	// Tokens this adapter has never heard of must not overwrite or destroy.
	for _, unknown := range []string{"objectLockToken", "someNewSubresource", "checksum", "select-type"} {
		if body, status := s3Put(t, base+"/fc/keep.txt?"+unknown+"=1", []byte("OVERWRITTEN"), now); status != 501 {
			t.Fatalf("PUT ?%s -> status %d, want 501; body %s", unknown, status, body)
		}
		resp := s3Delete(t, base+"/fc/keep.txt?"+unknown+"=1", now)
		dbody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 501 {
			t.Fatalf("DELETE ?%s -> status %d, want 501; body %s", unknown, resp.StatusCode, dbody)
		}
		if body, status := s3Get(t, base+"/fc/keep.txt", now); status != 200 || !strings.Contains(body, "hello") {
			t.Fatalf("?%s mutated the object -> status %d; body %s", unknown, status, body)
		}
	}

	// The parameters a real client legitimately sends on those routes still work.
	if body, status := s3Put(t, base+"/fc/plain.txt?x-id=PutObject", []byte("hello"), now); status != 200 {
		t.Fatalf("PUT with x-id -> status %d, want 200; body %s", status, body)
	}
	if body, status := s3Put(t, base+"/fc/keep.txt?x-id=PutObject", []byte("hello"), now); status != 200 {
		t.Fatalf("PUT over existing key with x-id -> status %d, want 200; body %s", status, body)
	}
	resp := s3Delete(t, base+"/fc/plain.txt?x-id=DeleteObject", now)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE with x-id -> status %d, want 204", resp.StatusCode)
	}

	// The bucket mutating routes are fail-closed the same way.
	if body, status := s3Put(t, base+"/fcghost?someNewSubresource=1", nil, now); status != 501 {
		t.Fatalf("PUT bucket with unknown token -> status %d, want 501; body %s", status, body)
	}
	if body, status := s3Get(t, base+"/fcghost?location", now); status != 404 {
		t.Fatalf("unknown bucket token created a bucket -> status %d; body %s", status, body)
	}
}

// TestAWSS3PresignedAuthParamsSurviveGuards pins that the parameters a SigV4
// presigner puts in the query string are not treated as subresources by the
// fail-closed guards. The requests here carry a valid header Authorization, so
// this exercises the guard's parameter handling rather than presigned
// verification itself: presigned GET is known-broken on this adapter (it
// answers 403 SignatureDoesNotMatch, identically on the pre-fix commit), which
// is out of scope here. What these cases pin is that the guards do not treat
// the presigned parameters as subresources.
func TestAWSS3PresignedAuthParamsSurviveGuards(t *testing.T) {
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

	if _, status := s3Put(t, base+"/presign", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if _, status := s3Put(t, base+"/presign/keep.txt", []byte("hello"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	// Every parameter a SigV4 presigner puts in the query, and nothing else.
	presignedQuery := "X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20260120%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20260120T120000Z" +
		"&X-Amz-Expires=900" +
		"&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=" + strings.Repeat("a", 64)

	// The guard must not fire: these carry every presigned-auth parameter a
	// SigV4 presigner emits, alongside a legitimate route parameter, and the
	// operation must still be performed rather than refused as a subresource.
	if body, status := s3Put(t, base+"/presign/keep.txt?"+presignedQuery+"&x-id=PutObject", []byte("hello"), now); status != 200 {
		t.Fatalf("PUT with presigned auth params -> status %d, want 200; body %s", status, body)
	}
	if body, status := s3Put(t, base+"/presign/second.txt?"+presignedQuery+"&x-id=PutObject", []byte("hello"), now); status != 200 {
		t.Fatalf("PUT new key with presigned auth params -> status %d, want 200; body %s", status, body)
	}
	resp := s3Delete(t, base+"/presign/second.txt?"+presignedQuery+"&x-id=DeleteObject", now)
	dbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE with presigned auth params -> status %d, want 204; body %s", resp.StatusCode, dbody)
	}

	// Bucket routes too: presigned-auth parameters must not be read as a
	// subresource, so the create proceeds. A bare presigned PUT carries nothing
	// but auth, which is what real CreateBucket accepts.
	if body, status := s3Put(t, base+"/presignghost?"+presignedQuery, nil, now); status != 200 {
		t.Fatalf("presigned bucket create -> status %d, want 200; body %s", status, body)
	}
	if body, status := s3Get(t, base+"/presignghost?location", now); status != 200 {
		t.Fatalf("presigned bucket create did not take effect -> status %d; body %s", status, body)
	}
	// A real subresource alongside the presigned parameters is still refused.
	if body, status := s3Put(t, base+"/presignother?"+presignedQuery+"&versioning", nil, now); status != 501 {
		t.Fatalf("presigned create with a real subresource -> status %d, want 501; body %s", status, body)
	}
}

// TestAWSS3XIdMustNameAnImplementedOperation pins that the mutating object
// routes validate the VALUE of x-id, not just the key. An AWS SDK sends
// `x-id=<Operation>` on every call, so allowing the key by name is not enough:
// `PUT /{b}/{k}?x-id=CopyObject` and `?x-id=RestoreObject` would otherwise
// reach PutObject and mutate the object behind an operation this adapter does
// not implement.
func TestAWSS3XIdMustNameAnImplementedOperation(t *testing.T) {
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

	if _, status := s3Put(t, base+"/xid", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if _, status := s3Put(t, base+"/xid/keep.txt", []byte("ORIGINAL"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	// Operations the adapter does not implement, on both mutating verbs.
	for _, op := range []string{
		"CopyObject", "RestoreObject", "SelectObjectContent", "PutObjectLegalHold",
		"GetObjectAttributes", "PutObjectTagging", "UploadPartCopy", "ObjectLockToken",
	} {
		if body, status := s3Put(t, base+"/xid/keep.txt?x-id="+op, []byte("OVERWRITTEN"), now); status != 501 {
			t.Fatalf("PUT ?x-id=%s -> status %d, want 501; body %s", op, status, body)
		}
		if b, st := s3Get(t, base+"/xid/keep.txt", now); st != 200 || !strings.Contains(b, "ORIGINAL") {
			t.Fatalf("PUT ?x-id=%s mutated the object -> status %d; body %s", op, st, b)
		}
		resp := s3Delete(t, base+"/xid/keep.txt?x-id="+op, now)
		dbody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 501 {
			t.Fatalf("DELETE ?x-id=%s -> status %d, want 501; body %s", op, resp.StatusCode, dbody)
		}
		if b, st := s3Get(t, base+"/xid/keep.txt", now); st != 200 || !strings.Contains(b, "ORIGINAL") {
			t.Fatalf("DELETE ?x-id=%s destroyed the object -> status %d; body %s", op, st, b)
		}
		// It must not create a key either.
		if body, status := s3Put(t, base+"/xid/ghost.txt?x-id="+op, []byte("X"), now); status != 501 {
			t.Fatalf("PUT ghost ?x-id=%s -> status %d, want 501; body %s", op, status, body)
		}
		if b, st := s3Get(t, base+"/xid/ghost.txt", now); st != 404 {
			t.Fatalf("PUT ghost ?x-id=%s created a key -> status %d; body %s", op, st, b)
		}
	}

	// The operations this adapter does implement still work, including mixed
	// casing on the key and the value.
	for _, ok := range []string{"x-id=PutObject", "X-Id=PutObject", "x-id=putobject"} {
		if body, status := s3Put(t, base+"/xid/keep.txt?"+ok, []byte("ORIGINAL"), now); status != 200 {
			t.Fatalf("PUT ?%s -> status %d, want 200; body %s", ok, status, body)
		}
	}
	resp := s3Delete(t, base+"/xid/keep.txt?x-id=DeleteObject", now)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE ?x-id=DeleteObject -> status %d, want 204", resp.StatusCode)
	}
}

// TestAWSS3PartNumberRequiresUploadId pins real S3's rule that partNumber only
// selects UploadPart when it accompanies uploadId. Without it,
// `PUT /{bucket}/{key}?partNumber=N` fell through to PutObject and overwrote
// the object.
func TestAWSS3PartNumberRequiresUploadId(t *testing.T) {
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

	if _, status := s3Put(t, base+"/pn", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if _, status := s3Put(t, base+"/pn/keep.txt", []byte("ORIGINAL"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	for _, q := range []string{"partNumber=1", "partNumber", "PARTNUMBER=2"} {
		body, status := s3Put(t, base+"/pn/keep.txt?"+q, []byte("OVERWRITTEN"), now)
		if status != 400 || !strings.Contains(body, "InvalidRequest") {
			t.Fatalf("PUT ?%s -> status %d, want 400 InvalidRequest; body %s", q, status, body)
		}
		if b, st := s3Get(t, base+"/pn/keep.txt", now); st != 200 || !strings.Contains(b, "ORIGINAL") {
			t.Fatalf("PUT ?%s overwrote the object -> status %d; body %s", q, st, b)
		}
	}

	// The real multipart pairing still works.
	body, status := s3Post(t, base+"/pn/multi.bin?uploads", nil, now)
	if status != 200 {
		t.Fatalf("initiate upload -> status %d, want 200; body %s", status, body)
	}
	uploadID := s3XMLTag(t, body, "UploadId")
	rawETag, partStatus := s3PutETag(t, base+"/pn/multi.bin?uploadId="+uploadID+"&partNumber=1", []byte("hello"), now)
	if partStatus != 200 {
		t.Fatalf("upload part -> status %d, want 200", partStatus)
	}
	if body, status := s3Post(t, base+"/pn/multi.bin?uploadId="+uploadID, []byte(s3CompleteBody([][2]string{{"1", strings.Trim(rawETag, `"`)}})), now); status != 200 {
		t.Fatalf("complete upload -> status %d, want 200; body %s", status, body)
	}
	if body, status := s3Get(t, base+"/pn/multi.bin", now); status != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("completed object -> status %d; body %s", status, body)
	}
}

// TestAWSS3MultipartRoutesAreGuarded pins that the multipart dispatch cannot be
// reached with a subresource the allowlist should have refused. A real SDK asks
// for UploadPartCopy as `?partNumber=N&uploadId=U&x-id=UploadPartCopy`; with the
// guard running after the dispatch that stored a zero-byte part and returned a
// successful response with no result, instead of refusing the operation.
func TestAWSS3MultipartRoutesAreGuarded(t *testing.T) {
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

	if _, status := s3Put(t, base+"/mpug", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	body, status := s3Post(t, base+"/mpug/multi.bin?uploads", nil, now)
	if status != 200 {
		t.Fatalf("initiate upload -> status %d, want 200; body %s", status, body)
	}
	uploadID := s3XMLTag(t, body, "UploadId")
	if uploadID == "" {
		t.Fatalf("no UploadId in %s", body)
	}

	// UploadPartCopy is the real-SDK shape: it must be refused, not stored as a
	// zero-byte UploadPart.
	for _, q := range []string{
		"uploadId=" + uploadID + "&partNumber=1&x-id=UploadPartCopy",
		"partNumber=1&uploadId=" + uploadID + "&x-id=UploadPartCopy",
		"uploadId=" + uploadID + "&partNumber=1&x-id=CopyObject",
		"uploadId=" + uploadID + "&partNumber=1&tagging",
		"uploadId=" + uploadID + "&partNumber=1&acl",
		"uploadId=" + uploadID + "&partNumber=1&retention",
	} {
		body, status := s3Put(t, base+"/mpug/multi.bin?"+q, []byte("hello"), now)
		if status != 501 {
			t.Fatalf("PUT ?%s -> status %d, want 501; body %s", q, status, body)
		}
	}
	// No part was stored by any of them.
	if body, status := s3Get(t, base+"/mpug/multi.bin?uploadId="+uploadID, now); status != 200 || strings.Contains(body, "<Part>") {
		t.Fatalf("guarded part uploads stored a part: %s", body)
	}

	// POST /{bucket}/{key} with a refused subresource must not create or
	// complete an upload.
	for _, q := range []string{
		"uploadId=" + uploadID + "&tagging",
		"uploadId=" + uploadID + "&acl",
		"uploadId=" + uploadID + "&x-id=CopyObject",
		"uploadId=" + uploadID + "&x-id=RestoreObject",
		"uploads&tagging",
		"uploads&x-id=SelectObjectContent",
	} {
		if body, status := s3Post(t, base+"/mpug/multi.bin?"+q, nil, now); status != 501 {
			t.Fatalf("POST ?%s -> status %d, want 501; body %s", q, status, body)
		}
	}
	// The upload is untouched: still one upload, still no parts, and the object
	// was never created.
	if body, status := s3Get(t, base+"/mpug/multi.bin?uploadId="+uploadID, now); status != 200 {
		t.Fatalf("list parts -> status %d, want 200; body %s", status, body)
	}
	if body, status := s3Get(t, base+"/mpug/multi.bin", now); status != 404 {
		t.Fatalf("guarded POST completed the upload -> status %d, want 404; body %s", status, body)
	}

	// The legitimate multipart lifecycle still works end to end.
	rawETag, partStatus := s3PutETag(t, base+"/mpug/multi.bin?uploadId="+uploadID+"&partNumber=1", []byte("hello"), now)
	if partStatus != 200 {
		t.Fatalf("upload part -> status %d, want 200", partStatus)
	}
	if body, status := s3Post(t, base+"/mpug/multi.bin?uploadId="+uploadID, []byte(s3CompleteBody([][2]string{{"1", strings.Trim(rawETag, `"`)}})), now); status != 200 {
		t.Fatalf("complete upload -> status %d, want 200; body %s", status, body)
	}
	if body, status := s3Get(t, base+"/mpug/multi.bin", now); status != 200 || !strings.Contains(body, "hello") {
		t.Fatalf("completed object -> status %d; body %s", status, body)
	}

	// An any-cased ?uploadId must not bypass the allowlist and overwrite an
	// object; the dispatch is case-insensitive so it reaches the guard instead.
	if _, status := s3Put(t, base+"/mpug/plain.txt", []byte("ORIGINAL"), now); status != 200 {
		t.Fatalf("put plain object -> status %d, want 200", status)
	}
	for _, q := range []string{"partNumber=1&UPLOADID=nosuch", "partNumber=1&uploadid=nosuch", "PARTNUMBER=1&uploadId=nosuch"} {
		if body, status := s3Put(t, base+"/mpug/plain.txt?"+q, []byte("CLOBBER"), now); status == 200 {
			t.Fatalf("PUT ?%s -> 200, want a refusal; body %s", q, body)
		}
		if b, st := s3Get(t, base+"/mpug/plain.txt", now); st != 200 || !strings.Contains(b, "ORIGINAL") {
			t.Fatalf("PUT ?%s overwrote the object -> status %d; body %s", q, st, b)
		}
		resp := s3Delete(t, base+"/mpug/plain.txt?"+q, now)
		resp.Body.Close()
		if resp.StatusCode == 204 {
			t.Fatalf("DELETE ?%q destroyed the object", q)
		}
	}
}

// TestAWSS3CompleteAcceptsXMLEscapedETags pins that CompleteMultipartUpload
// accepts an ETag however the client quoted it. Go's encoding/xml escapes the
// quotes around an ETag as `&#34;` and .NET's XmlWriter as `&quot;`, so a real
// SDK echoing a stored ETag back does not send literal quotes — and reading the
// entity verbatim made every Complete answer 400 InvalidPart.
func TestAWSS3CompleteAcceptsXMLEscapedETags(t *testing.T) {
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

	if _, status := s3Put(t, base+"/esc", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}

	// Every quoting form an SDK might send, including the ones the engine's own
	// CompleteMultipartUploadResult emits back to a client that re-serializes it.
	for _, form := range []struct{ name, wrap string }{
		{"literal quotes", `"%s"`},
		{"unquoted", `%s`},
		{"numeric character reference", `&#34;%s&#34;`},
		{"named quot entity", `&quot;%s&quot;`},
		{"weak validator", `W/"%s"`},
		{"uppercase hex", `"%s"`},
	} {
		key := form.name
		key = strings.ReplaceAll(key, " ", "")
		body, status := s3Post(t, base+"/esc/"+key+"?uploads", nil, now)
		if status != 200 {
			t.Fatalf("initiate %s -> status %d, want 200; body %s", form.name, status, body)
		}
		uploadID := s3XMLTag(t, body, "UploadId")
		rawETag, partStatus := s3PutETag(t, base+"/esc/"+key+"?uploadId="+uploadID+"&partNumber=1", []byte("hello"), now)
		if partStatus != 200 {
			t.Fatalf("upload part %s -> status %d, want 200", form.name, partStatus)
		}
		hexETag := strings.Trim(rawETag, `"`)
		if form.name == "uppercase hex" {
			hexETag = strings.ToUpper(hexETag)
		}
		complete := "<CompleteMultipartUpload><Part><ETag>" + fmt.Sprintf(form.wrap, hexETag) + "</ETag><PartNumber>1</PartNumber></Part></CompleteMultipartUpload>"
		if body, status := s3Post(t, base+"/esc/"+key+"?uploadId="+uploadID, []byte(complete), now); status != 200 {
			t.Fatalf("complete with %s -> status %d, want 200; body %s", form.name, status, body)
		}
		if body, status := s3Get(t, base+"/esc/"+key, now); status != 200 || !strings.Contains(body, "hello") {
			t.Fatalf("object after %s -> status %d; body %s", form.name, status, body)
		}
	}
}

// TestAWSS3ListObjectsV2Scales pins that a single bucket listing can return a
// few hundred objects. _xml_escape runs per listed entry, so an unrelated change
// to it silently moves this ceiling: an inlined control-character check once
// cost ~40% of the per-entry budget and cut the ceiling from ~290 objects to
// ~163, which surfaced as a JSON 500 instead of a listing.
func TestAWSS3ListObjectsV2Scales(t *testing.T) {
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

	const objects = 250
	if _, status := s3Put(t, base+"/scale", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	for i := 0; i < objects; i++ {
		if _, status := s3Put(t, base+"/scale/"+fmt.Sprintf("dir/prefix/key-%04d.bin", i), []byte("payload"), now); status != 200 {
			t.Fatalf("put object %d -> status %d, want 200", i, status)
		}
	}

	body, status := s3Get(t, base+"/scale?list-type=2", now)
	if status != 200 {
		t.Fatalf("list %d objects -> status %d, want 200 (step budget?); body %.400s", objects, status, body)
	}
	if got := strings.Count(body, "<Key>"); got != objects {
		t.Fatalf("listing returned %d keys, want %d", got, objects)
	}
	if strings.Contains(body, "too many steps") {
		t.Fatal("listing exhausted the Starlark step budget")
	}
}

// TestAWSS3DeleteObjectAcceptsVersionId pins that a versioned delete is not
// refused. The real SDK always sends `?versionId` on DeleteObject, and on a
// bucket with no versioning any id addresses the single object, which is what
// real S3 does. The query allowlist rejected it, regressing every versioned
// delete from 204 to 501.
//
// Like TestAWSS3BucketDeleteRefusedUntilEmpty, this passes on the pre-fix
// commit: base had no allowlist, so it already answered 204. The test guards
// against the allowlist drifting away from that correct behavior rather than
// introducing it.
func TestAWSS3DeleteObjectAcceptsVersionId(t *testing.T) {
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

	if _, status := s3Put(t, base+"/vid", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}
	if _, status := s3Put(t, base+"/vid/a.txt", []byte("hello"), now); status != 200 {
		t.Fatalf("put object -> status %d, want 200", status)
	}

	// The exact shape the Go SDK sends.
	for _, q := range []string{
		"x-id=DeleteObject&versionId=null",
		"versionId=null",
		"VERSIONID=null",
		"versionId=abc123",
	} {
		if _, status := s3Put(t, base+"/vid/a.txt", []byte("hello"), now); status != 200 {
			t.Fatalf("setup put -> status %d, want 200", status)
		}
		resp := s3Delete(t, base+"/vid/a.txt?"+q, now)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 204 {
			t.Fatalf("DELETE ?%s -> status %d, want 204; body %s", q, resp.StatusCode, body)
		}
		if b, st := s3Get(t, base+"/vid/a.txt", now); st != 404 {
			t.Fatalf("DELETE ?%s did not remove the object -> status %d; body %s", q, st, b)
		}
	}
}

// TestAWSS3CompleteRejectsDoubleEscapedETags pins that the requested ETag is
// XML-decoded exactly once. Decoding it in both the parser and the comparison
// accepted double-escaped quoting (`&amp;quot;H&amp;quot;`) that real S3, which
// decodes once, rejects with InvalidPart.
func TestAWSS3CompleteRejectsDoubleEscapedETags(t *testing.T) {
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

	if _, status := s3Put(t, base+"/deesc", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}

	for i, form := range []string{
		`&#34;%s&#34;`,   // Go encoding/xml
		`&quot;%s&quot;`, // .NET XmlWriter
		`"%s"`,           // literal quotes
	} {
		key := "single" + strconv.Itoa(i)
		body, status := s3Post(t, base+"/deesc/"+key+"?uploads", nil, now)
		if status != 200 {
			t.Fatalf("initiate %s -> status %d, want 200; body %s", form, status, body)
		}
		uploadID := s3XMLTag(t, body, "UploadId")
		rawETag, partStatus := s3PutETag(t, base+"/deesc/"+key+"?uploadId="+uploadID+"&partNumber=1", []byte("hello"), now)
		if partStatus != 200 {
			t.Fatalf("upload part -> status %d, want 200", partStatus)
		}
		hexETag := strings.Trim(rawETag, `"`)
		complete := "<CompleteMultipartUpload><Part><ETag>" + fmt.Sprintf(form, hexETag) + "</ETag><PartNumber>1</PartNumber></Part></CompleteMultipartUpload>"
		if body, status := s3Post(t, base+"/deesc/"+key+"?uploadId="+uploadID, []byte(complete), now); status != 200 {
			t.Fatalf("complete with %s -> status %d, want 200; body %s", form, status, body)
		}
	}

	// Double-escaped forms must be rejected: one decode leaves an entity
	// reference where real S3 expects the value.
	for _, form := range []string{`&amp;quot;%s&amp;quot;`, `&#38;quot;%s&#38;quot;`, `&#38;#34;%s&#38;#34;`} {
		body, status := s3Post(t, base+"/deesc/bad?uploads", nil, now)
		if status != 200 {
			t.Fatalf("initiate upload -> status %d, want 200; body %s", status, body)
		}
		uploadID := s3XMLTag(t, body, "UploadId")
		rawETag, partStatus := s3PutETag(t, base+"/deesc/bad?uploadId="+uploadID+"&partNumber=1", []byte("hello"), now)
		if partStatus != 200 {
			t.Fatalf("upload part -> status %d, want 200", partStatus)
		}
		complete := "<CompleteMultipartUpload><Part><ETag>" + fmt.Sprintf(form, strings.Trim(rawETag, `"`)) + "</ETag><PartNumber>1</PartNumber></Part></CompleteMultipartUpload>"
		if body, status := s3Post(t, base+"/deesc/bad?uploadId="+uploadID, []byte(complete), now); status != 400 || !strings.Contains(body, "InvalidPart") {
			t.Fatalf("complete with double-escaped %s -> status %d, want 400 InvalidPart; body %s", form, status, body)
		}
	}
}

// TestAWSS3NonASCIIKeysRoundTrip pins that a bucket or key holding any
// non-ASCII byte works. The SigV4 canonical URI percent-encodes the decoded
// path, and it used to read each byte with ord() — which returns U+FFFD for a
// lone byte >= 0x80, because a single byte is not valid UTF-8. That indexed a
// 16-character hex table at 4095 and every such request died with
// `string index 4095 out of range`, including keys that are perfectly valid
// UTF-8.
func TestAWSS3NonASCIIKeysRoundTrip(t *testing.T) {
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

	// The bucket itself carries a non-ASCII byte.
	const bucket = "caf%C3%A9-bucket"
	if _, status := s3Put(t, base+"/"+bucket, nil, now); status != 200 {
		t.Fatalf("create non-ascii bucket -> status %d, want 200", status)
	}

	keys := []struct{ name, esc, want string }{
		{"latin-1 supplement", "caf%C3%A9.txt", "café.txt"},
		{"latin extended", "na%C3%AFve.txt", "naïve.txt"},
		{"cjk", "%E6%97%A5%E6%9C%AC.txt", "日本.txt"},
		{"emoji outside the BMP", "box%F0%9F%93%A6.txt", "box📦.txt"},
		{"two-byte and space", "a%20%C3%A9b.txt", "a éb.txt"},
		{"ascii control", "tab%09here.txt", "tab\there.txt"},
	}
	for _, k := range keys {
		if _, status := s3Put(t, base+"/"+bucket+"/"+k.esc, []byte("payload-"+k.name), now); status != 200 {
			t.Fatalf("put %s -> status %d, want 200", k.name, status)
		}
		body, status := s3Get(t, base+"/"+bucket+"/"+k.esc, now)
		if status != 200 {
			t.Fatalf("get %s -> status %d, want 200; body %s", k.name, status, body)
		}
		if body != "payload-"+k.name {
			t.Fatalf("get %s -> %q, want %q", k.name, body, "payload-"+k.name)
		}
		// HEAD must agree, so the stored key is byte-exact.
		resp := s3Head(t, base+"/"+bucket+"/"+k.esc, now)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("head %s -> status %d, want 200", k.name, resp.StatusCode)
		}
	}

	// A listing must round-trip the key name, not a replacement character.
	body, status := s3Get(t, base+"/"+bucket+"?list-type=2", now)
	if status != 200 {
		t.Fatalf("list -> status %d, want 200; body %s", status, body)
	}
	for _, k := range keys {
		if !strings.Contains(body, "<Key>"+k.want+"</Key>") {
			t.Fatalf("listing lost %s: %s", k.name, body)
		}
		if strings.Contains(body, "�") {
			t.Fatalf("listing contains a replacement character: %s", body)
		}
	}

	// encoding-type=url percent-encodes keys in the response; it must encode
	// the real bytes, not the UTF-8 of U+FFFD.
	body, status = s3Get(t, base+"/"+bucket+"?list-type=2&encoding-type=url", now)
	if status != 200 {
		t.Fatalf("list with encoding-type=url -> status %d; body %s", status, body)
	}
	if !strings.Contains(body, "caf%C3%A9.txt") {
		t.Fatalf("encoding-type=url did not encode the real bytes: %s", body)
	}
	if strings.Contains(body, "%EF%BF%BD") {
		t.Fatalf("encoding-type=url emitted a replacement character: %s", body)
	}

	// And the non-ASCII bucket can be deleted, which also signs its path.
	resp := s3Delete(t, base+"/"+bucket, now)
	if resp.StatusCode != 409 {
		t.Fatalf("delete non-empty non-ascii bucket -> status %d, want 409", resp.StatusCode)
	}
	resp = s3Delete(t, base+"/"+bucket+"/caf%C3%A9.txt", now)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("delete non-ascii key -> status %d, want 204", resp.StatusCode)
	}
}
