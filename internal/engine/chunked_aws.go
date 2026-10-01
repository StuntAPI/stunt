// AWS-chunked (SigV4 STREAMING) framing decode.
//
// S3 SDKs using SigV4 streaming uploads send the object bytes wrapped in
// aws-chunked framing: hex-size chunk lines with opaque ";extensions",
// strict CRLF delimiters, a terminal zero chunk, and optional trailers.
// The engine strips that framing before adapter dispatch so handlers see
// the decoded bytes, while the Content-Encoding and x-amz-content-sha256
// headers are preserved untouched (SigV4 signs them).
package engine

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
)

const (
	// awsChunkedReadOverhead is the headroom above bodyLimit granted to
	// the encoded (still framed) read: framing, extensions, and trailers
	// for up to awsChunkedMaxChunks chunks.
	awsChunkedReadOverhead = int64(2 << 20) // 2 MiB
	// awsChunkedMaxLine caps one chunk-size or trailer line (incl CRLF).
	awsChunkedMaxLine = 4096
	// awsChunkedMaxChunks caps data chunks per request.
	awsChunkedMaxChunks = 10000
	// awsChunkedMaxTrailers caps trailer header lines after the zero chunk.
	awsChunkedMaxTrailers = 32
	// awsChunkedMaxTrailerBytes caps total trailer line bytes (incl CRLF).
	awsChunkedMaxTrailerBytes = 8 << 10
)

// awsChunkedError is a decode failure: status is 400 (framing) or 413
// (decoded/encoded over limit). The 400 body carries the S3 IncompleteBody
// code; the error is never echoed back with request bytes.
type awsChunkedError struct {
	status int
}

func (e *awsChunkedError) Error() string {
	if e.status == http.StatusRequestEntityTooLarge {
		return "aws-chunked decoded body exceeds limit"
	}
	return "IncompleteBody"
}

// writeIncompleteBody writes the S3 IncompleteBody error envelope for
// aws-chunked framing failures. The request bytes are never echoed.
func writeIncompleteBody(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>IncompleteBody</Code><Message>The request body terminated unexpectedly or did not match the expected length.</Message></Error>`))
}

// flattenHeaderTokens flattens multi-value headers into comma-split,
// OWS-trimmed tokens (empty members dropped per RFC 9110 list parsing).
func flattenHeaderTokens(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, tok := range strings.Split(v, ",") {
			tok = strings.Trim(tok, " \t")
			if tok == "" {
				continue
			}
			out = append(out, tok)
		}
	}
	return out
}

// hasAwsChunkedToken reports whether any Content-Encoding value carries
// the aws-chunked token (comma-split, OWS-trimmed, case-folded).
func hasAwsChunkedToken(vals []string) bool {
	for _, tok := range flattenHeaderTokens(vals) {
		if strings.ToLower(tok) == "aws-chunked" {
			return true
		}
	}
	return false
}

// hasStreamingPayloadToken reports whether any x-amz-content-sha256 value
// is a STREAMING-* token (malformed-client trigger without Content-Encoding).
func hasStreamingPayloadToken(vals []string) bool {
	for _, tok := range flattenHeaderTokens(vals) {
		if strings.HasPrefix(strings.ToLower(tok), "streaming-") {
			return true
		}
	}
	return false
}

// shouldDecodeAwsChunked reports whether the request asks for aws-chunked
// decoding: an aws-chunked Content-Encoding token, or a STREAMING-*
// content hash without Content-Encoding. Uses Header.Values so split
// multi-value headers are all visible (headerMap keeps first-only).
func shouldDecodeAwsChunked(h http.Header) bool {
	return hasAwsChunkedToken(h.Values("Content-Encoding")) ||
		hasStreamingPayloadToken(h.Values("X-Amz-Content-Sha256"))
}

// parseDecodedContentLength flattens x-amz-decoded-content-length values:
// exactly one valid decimal token verifies against the decoded length,
// zero/multiple/invalid tokens are ignored.
func parseDecodedContentLength(vals []string) (uint64, bool) {
	toks := flattenHeaderTokens(vals)
	if len(toks) != 1 {
		return 0, false
	}
	s := toks[0]
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// trimOWS trims spaces and horizontal tabs (HTTP optional whitespace).
func trimOWS(b []byte) []byte {
	return bytes.Trim(b, " \t")
}

// decodeAwsChunked strips SigV4 STREAMING framing from encoded, returning
// the decoded bytes. bodyLimit bounds the decoded total (exceeding it is
// a 413); every other framing violation is a 400 IncompleteBody.
//
// Grammar: strict CRLF (bare LF or lone CR fail); chunk sizes are
// OWS-trimmed hex (uppercase ok) with explicit rejection of +/- signs and
// 0x prefixes before ParseUint(16,64); everything after the first ';' is
// an opaque extension (bare sizes without extensions are accepted in both
// streaming variants). Chunk data is exactly size bytes plus CRLF. The
// terminal zero chunk may carry trailer lines (Name: value, split at the
// first colon, empty value ok) which are discarded, never merged into
// request headers; checksum trailers are discarded the same way. Caps:
// lines <= 4KB, data chunks <= 10k, trailers <= 32 lines / 8KB total,
// per-chunk and running decoded totals <= bodyLimit (incremental).
func decodeAwsChunked(encoded []byte, bodyLimit int64, decodedLengthVals []string) ([]byte, error) {
	fail := &awsChunkedError{status: http.StatusBadRequest}
	tooBig := &awsChunkedError{status: http.StatusRequestEntityTooLarge}

	var decoded []byte
	chunks := 0
	pos := 0
	// readLine consumes one strict-CRLF line (returned without the CRLF).
	readLine := func() ([]byte, error) {
		end := -1
		for i := pos; i < len(encoded); i++ {
			if encoded[i] == '\n' {
				end = i
				break
			}
		}
		if end < 0 {
			return nil, fail // unterminated
		}
		if end == pos || encoded[end-1] != '\r' {
			return nil, fail // bare LF
		}
		line := encoded[pos : end-1]
		if bytes.IndexByte(line, '\r') >= 0 {
			return nil, fail // lone CR
		}
		if end+1-pos > awsChunkedMaxLine {
			return nil, fail
		}
		pos = end + 1
		return line, nil
	}

	for {
		line, err := readLine()
		if err != nil {
			return nil, err
		}
		sizeField := line
		if i := bytes.IndexByte(line, ';'); i >= 0 {
			sizeField = line[:i]
		}
		sizeStr := trimOWS(sizeField)
		if len(sizeStr) == 0 {
			return nil, fail
		}
		if sizeStr[0] == '+' || sizeStr[0] == '-' {
			return nil, fail
		}
		if len(sizeStr) >= 2 && sizeStr[0] == '0' && (sizeStr[1] == 'x' || sizeStr[1] == 'X') {
			return nil, fail
		}
		size, err := strconv.ParseUint(string(sizeStr), 16, 64)
		if err != nil {
			return nil, fail
		}
		if size == 0 {
			trailerBytes := 0
			for n := 0; ; n++ {
				tline, err := readLine()
				if err != nil {
					return nil, err
				}
				if len(tline) == 0 {
					break // terminal empty line
				}
				if n+1 > awsChunkedMaxTrailers {
					return nil, fail
				}
				trailerBytes += len(tline) + 2 // raw line incl CRLF
				if trailerBytes > awsChunkedMaxTrailerBytes {
					return nil, fail
				}
				ci := bytes.IndexByte(tline, ':')
				if ci < 0 {
					return nil, fail // no colon
				}
				if len(trimOWS(tline[:ci])) == 0 {
					return nil, fail // empty name
				}
				// Value (possibly empty) is OWS-trimmed, then the
				// trailer is discarded — never merged into headers.
			}
			if pos != len(encoded) {
				return nil, fail // trailing garbage
			}
			break
		}
		// Incremental decoded bound before touching the data.
		chunks++
		if chunks > awsChunkedMaxChunks {
			return nil, fail
		}
		if bodyLimit < 0 || size > uint64(bodyLimit) || uint64(len(decoded)) > uint64(bodyLimit)-size {
			return nil, tooBig
		}
		remaining := uint64(len(encoded) - pos)
		if size+2 < size || size+2 > remaining {
			return nil, fail // truncated data
		}
		if encoded[pos+int(size)] != '\r' || encoded[pos+int(size)+1] != '\n' {
			return nil, fail
		}
		decoded = append(decoded, encoded[pos:pos+int(size)]...)
		pos += int(size) + 2
	}

	if want, ok := parseDecodedContentLength(decodedLengthVals); ok {
		if uint64(len(decoded)) != want {
			return nil, fail
		}
	}
	return decoded, nil
}
