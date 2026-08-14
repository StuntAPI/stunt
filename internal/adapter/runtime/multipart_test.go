package runtime

import (
	"bytes"
	"mime/multipart"
	"testing"

	sk "go.starlark.net/starlark"
)

// callMultipart invokes the parse_multipart builtin and returns (parts, err).
func callMultipart(t *testing.T, contentType, body string) (sk.Value, sk.Value) {
	t.Helper()
	b := BuildAllBuiltins(BuiltinOptions{})
	fn, ok := b["parse_multipart"]
	if !ok {
		t.Fatal("parse_multipart builtin not registered")
	}
	res, err := sk.Call(new(sk.Thread), fn, sk.Tuple{sk.String(contentType), sk.String(body)}, nil)
	if err != nil {
		t.Fatalf("parse_multipart: %v", err)
	}
	tup, ok := res.(sk.Tuple)
	if !ok || tup.Len() != 2 {
		t.Fatalf("parse_multipart returned %s, want 2-tuple", res.Type())
	}
	return tup.Index(0), tup.Index(1)
}

func partField(t *testing.T, part sk.Value, key string) sk.Value {
	t.Helper()
	d, ok := part.(*sk.Dict)
	if !ok {
		t.Fatalf("part is %s, want dict", part.Type())
	}
	v, found, _ := d.Get(sk.String(key))
	if !found {
		t.Fatalf("part has no %q field", key)
	}
	return v
}

// buildMultipartBody writes a multipart/form-data body with one file part and
// one plain field, returning the body and the full Content-Type header value.
func buildMultipartBody(t *testing.T, fileBytes []byte) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("type", "identity_document"); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("front", "front.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(fileBytes); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), buf.String()
}

func TestParseMultipartFieldsAndFile(t *testing.T) {
	fileBytes := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0xfe}
	ct, body := buildMultipartBody(t, fileBytes)

	parts, merr := callMultipart(t, ct, body)
	if merr != sk.None {
		t.Fatalf("err = %v, want None", merr)
	}
	l, ok := parts.(*sk.List)
	if !ok || l.Len() != 2 {
		t.Fatalf("parts = %s (len %d), want 2", parts.String(), l.Len())
	}

	field := l.Index(0)
	if got := partField(t, field, "name"); got.String() != `"type"` {
		t.Fatalf("field name = %s, want type", got.String())
	}
	if got := partField(t, field, "data"); got.String() != `"identity_document"` {
		t.Fatalf("field data = %s, want identity_document", got.String())
	}
	if got := partField(t, field, "filename"); got != sk.None {
		t.Fatalf("field filename = %v, want None", got)
	}

	file := l.Index(1)
	if got := partField(t, file, "name"); got.String() != `"front"` {
		t.Fatalf("file name = %s, want front", got.String())
	}
	if got := partField(t, file, "filename"); got.String() != `"front.png"` {
		t.Fatalf("file filename = %s, want front.png", got.String())
	}
	if got := partField(t, file, "content_type"); got.String() != `"application/octet-stream"` {
		t.Fatalf("file content_type = %s, want application/octet-stream", got.String())
	}
	// Binary data must round-trip byte-exact through the Starlark string.
	data, ok := partField(t, file, "data").(sk.String)
	if !ok || string(data) != string(fileBytes) {
		t.Fatalf("file data did not round-trip: %q", string(data))
	}
}

func TestParseMultipartEmptyBody(t *testing.T) {
	parts, merr := callMultipart(t, "multipart/form-data; boundary=X", "")
	if merr == sk.None {
		t.Fatal("empty body: want err, got None")
	}
	if parts != sk.None {
		t.Fatalf("empty body: parts = %v, want None", parts)
	}
}

func TestParseMultipartBadContentType(t *testing.T) {
	for _, ct := range []string{"application/json", "multipart/form-data", ""} {
		_, merr := callMultipart(t, ct, "x")
		if merr == sk.None {
			t.Fatalf("content-type %q: want err, got None", ct)
		}
	}
}
