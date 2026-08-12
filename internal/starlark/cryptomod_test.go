package starlark

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	sk "go.starlark.net/starlark"
)

// callCrypto invokes a crypto module member and returns its result.
func callCrypto(t *testing.T, name string, args []sk.Value, kwargs []sk.Tuple) sk.Value {
	t.Helper()
	fn, ok := cryptoModule.Members[name]
	if !ok {
		t.Fatalf("crypto.%s not found", name)
	}
	res, err := sk.Call(new(sk.Thread), fn, sk.Tuple(args), kwargs)
	if err != nil {
		t.Fatalf("crypto.%s: %v", name, err)
	}
	return res
}

func TestCryptoHMACSHA256(t *testing.T) {
	// Well-known vector: HMAC-SHA256("key", "The quick brown fox...").
	got := callCrypto(t, "hmac_sha256",
		[]sk.Value{sk.String("key"), sk.String("The quick brown fox jumps over the lazy dog")}, nil)
	const want = "f7bc83f430538424b13298e6aa6fb143ef4d59a14946175997479dbc2d1a3cd8"
	if string(got.(sk.String)) != want {
		t.Errorf("hmac_sha256 hex = %q, want %q", got, want)
	}

	// base64 encoding must be the standard-padded b64 of the same digest.
	mac := hmac.New(sha256.New, []byte("key"))
	mac.Write([]byte("The quick brown fox jumps over the lazy dog"))
	wantB64 := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	gotB64 := callCrypto(t, "hmac_sha256",
		[]sk.Value{sk.String("key"), sk.String("The quick brown fox jumps over the lazy dog")},
		[]sk.Tuple{{sk.String("encoding"), sk.String("base64")}})
	if string(gotB64.(sk.String)) != wantB64 {
		t.Errorf("hmac_sha256 base64 = %q, want %q", gotB64, wantB64)
	}
}

func TestCryptoHMACSHA1(t *testing.T) {
	// RFC 2202-style vector: HMAC-SHA1("key", "The quick brown fox...").
	got := callCrypto(t, "hmac_sha1",
		[]sk.Value{sk.String("key"), sk.String("The quick brown fox jumps over the lazy dog")}, nil)
	const want = "de7c9b85b8b78aa6bc8a7a36f70a90701c9db4d9"
	if string(got.(sk.String)) != want {
		t.Errorf("hmac_sha1 = %q, want %q", got, want)
	}
	// Cross-check against the Go implementation directly.
	mac := hmac.New(sha1.New, []byte("key"))
	mac.Write([]byte("The quick brown fox jumps over the lazy dog"))
	if string(got.(sk.String)) != bytesToHex(mac.Sum(nil)) {
		t.Errorf("hmac_sha1 disagrees with crypto/hmac")
	}
}

func TestCryptoSHA256(t *testing.T) {
	cases := map[string]string{
		"":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"abc": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	}
	for in, want := range cases {
		got := callCrypto(t, "sha256", []sk.Value{sk.String(in)}, nil)
		if string(got.(sk.String)) != want {
			t.Errorf("sha256(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCryptoBase64RoundTrip(t *testing.T) {
	enc := callCrypto(t, "base64_encode", []sk.Value{sk.String("foobar")}, nil)
	if string(enc.(sk.String)) != "Zm9vYmFy" {
		t.Errorf("base64_encode = %q, want Zm9vYmFy", enc)
	}
	dec := callCrypto(t, "base64_decode", []sk.Value{enc}, nil)
	if string(dec.(sk.String)) != "foobar" {
		t.Errorf("base64_decode round-trip = %q, want foobar", dec)
	}
}

func TestCryptoUnknownEncoding(t *testing.T) {
	fn := cryptoModule.Members["hmac_sha256"]
	_, err := sk.Call(new(sk.Thread), fn,
		sk.Tuple{sk.String("k"), sk.String("d")},
		[]sk.Tuple{{sk.String("encoding"), sk.String("rot13")}})
	if err == nil {
		t.Fatal("hmac_sha256 with unknown encoding should error")
	}
}

// bytesToHex is a tiny local helper to avoid pulling encoding/hex into the
// test imports just for one cross-check.
func bytesToHex(b []byte) string {
	const hexc = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[2*i] = hexc[v>>4]
		out[2*i+1] = hexc[v&0x0f]
	}
	return string(out)
}
