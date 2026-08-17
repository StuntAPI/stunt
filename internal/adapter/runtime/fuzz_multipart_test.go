package runtime

import (
	"sync"
	"testing"

	sk "go.starlark.net/starlark"
)

// The parse_multipart builtin is shared by every handler that decodes
// multipart/form-data, so it must be TOTAL over adversarial input: its
// contract is the in-band (parts, err) pair, and an eval-time raise would
// surface as a 500. One builtins dict per process is enough — it is
// stateless per call.
var (
	fuzzBuiltinsOnce sync.Once
	fuzzParseMulti   sk.Value
)

func fuzzMultipartFn() sk.Value {
	fuzzBuiltinsOnce.Do(func() {
		b := BuildAllBuiltins(BuiltinOptions{})
		fn, ok := b["parse_multipart"]
		if !ok {
			panic("parse_multipart builtin not registered")
		}
		fuzzParseMulti = fn
	})
	return fuzzParseMulti
}

// FuzzParseMultipart throws arbitrary Content-Type/body pairs at the
// multipart decoder. Invariants: no panic, no eval-time raise, and the
// result is the documented 2-tuple.
func FuzzParseMultipart(f *testing.F) {
	f.Add("multipart/form-data; boundary=----x",
		"------x\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nv\r\n------x--\r\n")
	f.Add("multipart/form-data", "no boundary param")
	f.Add("multipart/form-data; boundary=", "garbage")
	f.Add("", "")
	f.Add("multipart/mixed; boundary=b", "--b\r\n\r\nx\r\n--b--")
	f.Add("application/json", "{}")
	f.Add("multipart/form-data; boundary=🦀", "--🦀\r\nContent-Disposition: form-data; name=\"f\"; filename=\"x\"\r\n\r\nbytes\r\n--🦀--")
	f.Add("multipart/form-data; boundary=b",
		"--b\r\nContent-Disposition: form-data\r\n\r\nno name\r\n--b--")
	f.Add("multipart/form-data; boundary=b", "--b")
	f.Add("multipart/form-data; boundary=b", "--b--")
	f.Fuzz(func(t *testing.T, contentType, body string) {
		res, err := sk.Call(new(sk.Thread), fuzzMultipartFn(), sk.Tuple{sk.String(contentType), sk.String(body)}, nil)
		if err != nil {
			t.Fatalf("parse_multipart raised on ct=%q body=%q: %v (contract is in-band (parts, err))", contentType, body, err)
		}
		tup, ok := res.(sk.Tuple)
		if !ok || tup.Len() != 2 {
			t.Fatalf("parse_multipart returned %s, want 2-tuple", res.Type())
		}
	})
}
