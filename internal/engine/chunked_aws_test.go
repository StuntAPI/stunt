package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/manifest"
)

// awsChunkedFraming wraps data in SigV4 STREAMING (aws-chunked) framing:
// one data chunk plus the terminating zero chunk, with opaque
// chunk-signature extensions (per-chunk signatures are not verified,
// header SigV4 only).
func awsChunkedFraming(data []byte) []byte {
	sig := strings.Repeat("a", 64)
	var b strings.Builder
	fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(data), sig)
	b.Write(data)
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "0;chunk-signature=%s\r\n\r\n", sig)
	return []byte(b.String())
}

// s3SignedReqChunked builds a SigV4-signed PUT carrying aws-chunked
// framing, as real S3 SDKs do: Content-Encoding: aws-chunked, the
// x-amz-content-sha256 header carries the STREAMING literal, and the
// signature covers that literal (not the body bytes).
func s3SignedReqChunked(t *testing.T, rawurl string, framing []byte, decodedLen int, at time.Time) *http.Request {
	t.Helper()
	req, err := http.NewRequest("PUT", rawurl, bytes.NewReader(framing))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("x-amz-decoded-content-length", strconv.Itoa(decodedLen))
	const streamHash = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	req.Header.Set("x-amz-content-sha256", streamHash)
	awsSigV4SignPayload(t, req, streamHash, "s3", awsStyleAccessKey, awsStyleSecretKey, at)
	return req
}

// TestAWSChunkedDecode is the S3 5B→5B round-trip: a PUT carrying
// aws-chunked framing must be stored decoded, so GET returns the 5
// decoded bytes (not the framing) and HEAD reports ContentLength 5.
func TestAWSChunkedDecode(t *testing.T) {
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

	if _, status := s3Put(t, base+"/chunkbucket", nil, now); status != 200 {
		t.Fatalf("create bucket -> status %d, want 200", status)
	}

	framing := awsChunkedFraming([]byte("hello"))

	resp, err := http.DefaultClient.Do(s3SignedReqChunked(t, base+"/chunkbucket/hello.txt", framing, 5, now))
	if err != nil {
		t.Fatal(err)
	}
	putBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("chunked put -> status %d, want 200; body %s", resp.StatusCode, putBody)
	}
	// The ETag must be the MD5 of the decoded bytes, not of the chunk framing
	// and not of the signed payload. Asserted exactly: a shape check would pass
	// on a digest computed from the wrong input.
	wantETag := `"` + awsMD5Hex([]byte("hello")) + `"`
	if etag := resp.Header.Get("ETag"); etag != wantETag {
		t.Fatalf("chunked put ETag = %q, want %q (md5 of the decoded body)", etag, wantETag)
	}

	body, status := s3Get(t, base+"/chunkbucket/hello.txt", now)
	if status != 200 {
		t.Fatalf("get chunked object -> status %d, want 200; body %s", status, body)
	}
	if body != "hello" {
		t.Fatalf("chunked round-trip body = %q (%dB), want %q (5B)", body, len(body), "hello")
	}

	hresp := s3Head(t, base+"/chunkbucket/hello.txt", now)
	hresp.Body.Close()
	if hresp.StatusCode != 200 {
		t.Fatalf("head chunked object -> status %d, want 200", hresp.StatusCode)
	}
	if cl := hresp.Header.Get("Content-Length"); cl != "5" {
		t.Fatalf("rawHttpBodyLength %s, want 5", cl)
	}
	// HEAD must report the same validator the PUT returned. Without this, a
	// digest computed correctly for the response but stored wrongly on the
	// object would pass this test, and a client that caches the HEAD validator
	// would then send an If-Match that never matches.
	if etag := hresp.Header.Get("ETag"); etag != wantETag {
		t.Fatalf("HEAD ETag = %q, want %q (same digest the PUT returned)", etag, wantETag)
	}

	// Declared decoded length mismatch -> 400 IncompleteBody.
	resp, err = http.DefaultClient.Do(s3SignedReqChunked(t, base+"/chunkbucket/bad.txt", framing, 6, now))
	if err != nil {
		t.Fatal(err)
	}
	mismatchBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("decoded-length mismatch -> status %d, want 400; body %s", resp.StatusCode, mismatchBody)
	}
	if !strings.Contains(string(mismatchBody), "IncompleteBody") {
		t.Fatalf("decoded-length mismatch: missing IncompleteBody; body %s", mismatchBody)
	}

	// STREAMING sha256 without Content-Encoding and without framing
	// (malformed client) -> decode attempt -> 400 IncompleteBody.
	plain := []byte("hello")
	req, err := http.NewRequest("PUT", base+"/chunkbucket/plain.txt", bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	const streamHash = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	req.Header.Set("x-amz-content-sha256", streamHash)
	awsSigV4SignPayload(t, req, streamHash, "s3", awsStyleAccessKey, awsStyleSecretKey, now)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	plainBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("streaming-without-framing -> status %d, want 400; body %s", resp.StatusCode, plainBody)
	}
	if !strings.Contains(string(plainBody), "IncompleteBody") {
		t.Fatalf("streaming-without-framing: missing IncompleteBody; body %s", plainBody)
	}
}

// chunkedStatus decodes enc and returns the resulting status: 200 on
// success, else the aws-chunked error status (400 framing, 413 over
// limit).
func chunkedStatus(t *testing.T, enc []byte, bodyLimit int64, decLenVals []string) (int, []byte) {
	t.Helper()
	dec, err := decodeAwsChunked(enc, bodyLimit, decLenVals)
	if err == nil {
		return 200, dec
	}
	cerr, ok := err.(*awsChunkedError)
	if !ok {
		t.Fatalf("decode error type = %T, want *awsChunkedError", err)
	}
	return cerr.status, nil
}

func TestAWSChunkedParser(t *testing.T) {
	const limit = int64(1 << 20)

	t.Run("single chunk with extension", func(t *testing.T) {
		st, dec := chunkedStatus(t, []byte("5;chunk-signature=abc\r\nhello\r\n0;chunk-signature=abc\r\n\r\n"), limit, nil)
		if st != 200 || string(dec) != "hello" {
			t.Fatalf("status=%d body=%q, want 200 hello", st, dec)
		}
	})
	t.Run("multi chunk bare sizes", func(t *testing.T) {
		enc := []byte("5\r\nhello\r\n1\r\n \r\n1\r\n!\r\n0\r\n\r\n")
		st, dec := chunkedStatus(t, enc, limit, nil)
		if st != 200 || string(dec) != "hello !" {
			t.Fatalf("status=%d body=%q, want 200 'hello !'", st, dec)
		}
	})
	t.Run("uppercase hex ok", func(t *testing.T) {
		st, dec := chunkedStatus(t, []byte("A\r\n0123456789\r\n0\r\n\r\n"), limit, nil)
		if st != 200 || string(dec) != "0123456789" {
			t.Fatalf("status=%d body=%q, want 200", st, dec)
		}
	})
	t.Run("ows trim tab and space before semicolon", func(t *testing.T) {
		st, dec := chunkedStatus(t, []byte(" \t5 \t;chunk-signature=x\r\nhello\r\n0\r\n\r\n"), limit, nil)
		if st != 200 || string(dec) != "hello" {
			t.Fatalf("status=%d body=%q, want 200 hello", st, dec)
		}
	})
	t.Run("sign and hex prefix rejected", func(t *testing.T) {
		for _, enc := range []string{
			"+5\r\nhello\r\n0\r\n\r\n",
			"-5\r\nhello\r\n0\r\n\r\n",
			"0x5\r\nhello\r\n0\r\n\r\n",
			"0X5\r\nhello\r\n0\r\n\r\n",
			"\r\nhello\r\n0\r\n\r\n",
			"zz\r\nhello\r\n0\r\n\r\n",
		} {
			if st, _ := chunkedStatus(t, []byte(enc), limit, nil); st != 400 {
				t.Fatalf("%q -> status %d, want 400", enc, st)
			}
		}
	})
	t.Run("bare LF and lone CR rejected", func(t *testing.T) {
		for _, enc := range []string{
			"5;chunk-signature=abc\nhello\r\n0\r\n\r\n",    // bare LF ends size line
			"5;chunk-signature=a\rc\r\nhello\r\n0\r\n\r\n", // lone CR in size line
			"5\r\nhello\r\n0\r\n\n",                        // bare LF ends final empty line
		} {
			if st, _ := chunkedStatus(t, []byte(enc), limit, nil); st != 400 {
				t.Fatalf("%q -> status %d, want 400", enc, st)
			}
		}
	})
	t.Run("truncated and short data rejected", func(t *testing.T) {
		for _, enc := range []string{
			"5\r\nhello",             // truncated mid-data
			"5\r\nhell\r\n0\r\n\r\n", // data short, CRLF elsewhere
			"5\r\nhelloXX0\r\n\r\n",  // missing CRLF after data
			"5\r\nhello\r\n",         // missing final chunk
		} {
			if st, _ := chunkedStatus(t, []byte(enc), limit, nil); st != 400 {
				t.Fatalf("%q -> status %d, want 400", enc, st)
			}
		}
	})
	t.Run("zero chunk yields empty", func(t *testing.T) {
		st, dec := chunkedStatus(t, []byte("0\r\n\r\n"), limit, nil)
		if st != 200 || len(dec) != 0 {
			t.Fatalf("status=%d body=%q, want 200 empty", st, dec)
		}
	})
	t.Run("trailers discarded never merged", func(t *testing.T) {
		enc := []byte("5\r\nhello\r\n0\r\nX-Amz-Checksum-Crc32c: abc:def\r\nX-Empty:\r\n\r\n")
		st, dec := chunkedStatus(t, enc, limit, nil)
		if st != 200 || string(dec) != "hello" {
			t.Fatalf("status=%d body=%q, want 200 hello", st, dec)
		}
	})
	t.Run("trailer malformed rejected", func(t *testing.T) {
		for _, enc := range []string{
			"0\r\n: novalue\r\n\r\n", // empty name
			"0\r\nno-colon\r\n\r\n",  // no colon
			"0\r\n   \r\n\r\n",       // whitespace-only line (no colon)
		} {
			if st, _ := chunkedStatus(t, []byte(enc), limit, nil); st != 400 {
				t.Fatalf("%q -> status %d, want 400", enc, st)
			}
		}
	})
	t.Run("trailing garbage rejected", func(t *testing.T) {
		if st, _ := chunkedStatus(t, []byte("0\r\n\r\nzzz"), limit, nil); st != 400 {
			t.Fatalf("trailing garbage -> status %d, want 400", st)
		}
	})
	t.Run("decoded length verify", func(t *testing.T) {
		enc := []byte("5\r\nhello\r\n0\r\n\r\n")
		if st, _ := chunkedStatus(t, enc, limit, []string{"5"}); st != 200 {
			t.Fatalf("exact length -> status %d, want 200", st)
		}
		if st, _ := chunkedStatus(t, enc, limit, []string{" \t5\t "}); st != 200 {
			t.Fatalf("ows-padded length -> status %d, want 200", st)
		}
		if st, _ := chunkedStatus(t, enc, limit, []string{"6"}); st != 400 {
			t.Fatalf("mismatch -> status %d, want 400", st)
		}
		if st, _ := chunkedStatus(t, enc, limit, []string{"4"}); st != 400 {
			t.Fatalf("short mismatch -> status %d, want 400", st)
		}
		// Zero/multiple/invalid are ignored: decode succeeds.
		for _, vals := range [][]string{nil, {}, {""}, {"abc"}, {"5", "5"}, {"5,6"}, {"99999999999999999999999"}} {
			if st, _ := chunkedStatus(t, enc, limit, vals); st != 200 {
				t.Fatalf("vals %q -> status %d, want 200 (ignored)", vals, st)
			}
		}
	})
	t.Run("decoded over body limit", func(t *testing.T) {
		// Single chunk larger than the limit.
		if st, _ := chunkedStatus(t, []byte("5\r\nhello\r\n0\r\n\r\n"), 4, nil); st != 413 {
			t.Fatalf("single over limit -> status %d, want 413", st)
		}
		// Running total crosses the limit on the second chunk.
		enc := []byte("3\r\nabc\r\n3\r\ndef\r\n0\r\n\r\n")
		if st, _ := chunkedStatus(t, enc, 5, nil); st != 413 {
			t.Fatalf("running over limit -> status %d, want 413", st)
		}
		if st, _ := chunkedStatus(t, enc, 6, nil); st != 200 {
			t.Fatalf("exact limit -> status %d, want 200", st)
		}
	})
	t.Run("chunk count cap", func(t *testing.T) {
		var ok strings.Builder
		for i := 0; i < awsChunkedMaxChunks; i++ {
			ok.WriteString("1\r\na\r\n")
		}
		ok.WriteString("0\r\n\r\n")
		if st, dec := chunkedStatus(t, []byte(ok.String()), 1<<20, nil); st != 200 || len(dec) != awsChunkedMaxChunks {
			t.Fatalf("10000 chunks -> status %d len %d, want 200/%d", st, len(dec), awsChunkedMaxChunks)
		}
		var over strings.Builder
		for i := 0; i <= awsChunkedMaxChunks; i++ {
			over.WriteString("1\r\na\r\n")
		}
		over.WriteString("0\r\n\r\n")
		if st, _ := chunkedStatus(t, []byte(over.String()), 1<<20, nil); st != 400 {
			t.Fatalf("10001 chunks -> status %d, want 400", st)
		}
	})
	t.Run("trailer caps", func(t *testing.T) {
		var many strings.Builder
		many.WriteString("0\r\n")
		for i := 0; i <= awsChunkedMaxTrailers; i++ {
			fmt.Fprintf(&many, "X-T-%d: v\r\n", i)
		}
		many.WriteString("\r\n")
		if st, _ := chunkedStatus(t, []byte(many.String()), limit, nil); st != 400 {
			t.Fatalf("33 trailers -> status %d, want 400", st)
		}
		big := "0\r\nX-Big: " + strings.Repeat("v", 8<<10) + "\r\n\r\n"
		if st, _ := chunkedStatus(t, []byte(big), limit, nil); st != 400 {
			t.Fatalf("8KB trailer -> status %d, want 400", st)
		}
		long := strings.Repeat("x", 5000) + "\r\n"
		if st, _ := chunkedStatus(t, []byte(long), limit, nil); st != 400 {
			t.Fatalf("5KB size line -> status %d, want 400", st)
		}
	})
}

func TestAWSChunkedTrigger(t *testing.T) {
	ce := func(vals ...string) http.Header {
		h := http.Header{}
		for _, v := range vals {
			h.Add("Content-Encoding", v)
		}
		return h
	}
	cases := []struct {
		header http.Header
		want   bool
	}{
		{ce("aws-chunked"), true},
		{ce("AWS-Chunked"), true},
		{ce("gzip, Aws-Chunked"), true},
		{ce("aws-chunked", "gzip"), true}, // split multi-value header
		{ce("aws-chunked, aws-chunked"), true},
		{ce("gzip"), false},
		{ce("xaws-chunked"), false},
		{ce(""), false},
		{http.Header{}, false},
	}
	for i, c := range cases {
		if got := shouldDecodeAwsChunked(c.header); got != c.want {
			t.Fatalf("case %d (%q) -> %v, want %v", i, c.header.Values("Content-Encoding"), got, c.want)
		}
	}

	stream := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("X-Amz-Content-Sha256", v)
		}
		return h
	}
	if !shouldDecodeAwsChunked(stream("STREAMING-AWS4-HMAC-SHA256-PAYLOAD")) {
		t.Fatal("STREAMING- sha256 without CE must trigger decode")
	}
	if !shouldDecodeAwsChunked(stream("streaming-unsigned-payload-trailer")) {
		t.Fatal("lowercase streaming- token must trigger decode")
	}
	if shouldDecodeAwsChunked(stream("UNSIGNED-PAYLOAD")) {
		t.Fatal("UNSIGNED-PAYLOAD must not trigger decode")
	}
	if shouldDecodeAwsChunked(stream("")) {
		t.Fatal("absent sha256 must not trigger decode")
	}
}
