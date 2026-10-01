package starlark

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
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

func callMD5HexConcatRaw(t *testing.T, args []sk.Value, kwargs []sk.Tuple) (sk.Value, error) {
	t.Helper()
	fn, ok := cryptoModule.Members["md5_hex_concat"]
	if !ok {
		t.Fatalf("crypto.md5_hex_concat not found")
	}
	return sk.Call(new(sk.Thread), fn, sk.Tuple(args), kwargs)
}

func md5HexConcatWant(hexes []string) string {
	raw := make([]byte, 0, len(hexes)*16)
	for _, h := range hexes {
		b, err := hex.DecodeString(strings.ToLower(h))
		if err != nil {
			panic(err)
		}
		raw = append(raw, b...)
	}
	sum := md5.Sum(raw)
	return hex.EncodeToString(sum[:])
}

func TestMD5HexConcat(t *testing.T) {
	// Single valid 32-hex → hex out (md5("hello") digest as input element).
	single := "5d41402abc4b2a76b9719d911017c592"
	got, err := callMD5HexConcatRaw(t,
		[]sk.Value{sk.NewList([]sk.Value{sk.String(single)})}, nil)
	if err != nil {
		t.Fatalf("md5_hex_concat single: unexpected error: %v", err)
	}
	if got == sk.None {
		t.Fatal("md5_hex_concat single: want hex, got None")
	}
	if want := md5HexConcatWant([]string{single}); string(got.(sk.String)) != want {
		t.Errorf("md5_hex_concat single = %q, want %q", got, want)
	}

	// Uppercase accepted, normalized to same lowercase result.
	upper := "5D41402ABC4B2A76B9719D911017C592"
	gotUpper, err := callMD5HexConcatRaw(t,
		[]sk.Value{sk.NewList([]sk.Value{sk.String(upper)})}, nil)
	if err != nil {
		t.Fatalf("md5_hex_concat upper: unexpected error: %v", err)
	}
	if gotUpper == sk.None {
		t.Fatal("md5_hex_concat upper: want hex, got None")
	}
	if string(gotUpper.(sk.String)) != string(got.(sk.String)) {
		t.Errorf("md5_hex_concat upper = %q, want %q (lowercased)", gotUpper, got)
	}

	// Multi-element vector: concat binary then md5.
	multi := []string{single, "d41d8cd98f00b204e9800998ecf8427e"}
	gotMulti, err := callMD5HexConcatRaw(t,
		[]sk.Value{sk.NewList([]sk.Value{sk.String(multi[0]), sk.String(multi[1])})}, nil)
	if err != nil {
		t.Fatalf("md5_hex_concat multi: unexpected error: %v", err)
	}
	if want := md5HexConcatWant(multi); string(gotMulti.(sk.String)) != want {
		t.Errorf("md5_hex_concat multi = %q, want %q", gotMulti, want)
	}

	// Every shape mismatch must be total (None, never Error).
	noneCases := map[string]sk.Value{
		"empty list":    sk.NewList(nil),
		"None top":      sk.None,
		"string top":    sk.String(single),
		"int top":       sk.MakeInt(1),
		"dict top":      sk.NewDict(0),
		"tuple top":     sk.Tuple{sk.String(single)},
		"non-string":    sk.NewList([]sk.Value{sk.MakeInt(1)}),
		"none elem":     sk.NewList([]sk.Value{sk.None}),
		"empty string":  sk.NewList([]sk.Value{sk.String("")}),
		"short non-32":  sk.NewList([]sk.Value{sk.String("abc")}),
		"long 33":       sk.NewList([]sk.Value{sk.String(single + "0")}),
		"64-hex":        sk.NewList([]sk.Value{sk.String(strings.Repeat("0", 64))}),
		"non-hex":       sk.NewList([]sk.Value{sk.String(strings.Repeat("z", 32))}),
		"ws-padded":     sk.NewList([]sk.Value{sk.String(" " + single + " ")}),
		"ws-tab-padded": sk.NewList([]sk.Value{sk.String("\t" + single)}),
		"one bad elem": sk.NewList([]sk.Value{
			sk.String(single), sk.String(strings.Repeat("z", 32)),
		}),
	}
	for name, arg := range noneCases {
		v, err := callMD5HexConcatRaw(t, []sk.Value{arg}, nil)
		if err != nil {
			t.Errorf("md5_hex_concat %s: want None with nil error, got error %v", name, err)
			continue
		}
		if v != sk.None {
			t.Errorf("md5_hex_concat %s = %v, want None", name, v)
		}
	}

	// >10000 elements → None.
	big := make([]sk.Value, 10001)
	for i := range big {
		big[i] = sk.String(single)
	}
	v, err := callMD5HexConcatRaw(t, []sk.Value{sk.NewList(big)}, nil)
	if err != nil {
		t.Fatalf("md5_hex_concat >10000: unexpected error: %v", err)
	}
	if v != sk.None {
		t.Errorf("md5_hex_concat >10000 = %v, want None", v)
	}
	// Exactly 10000 valid elements must still hash (boundary).
	boundary := make([]sk.Value, 10000)
	for i := range boundary {
		boundary[i] = sk.String(single)
	}
	v, err = callMD5HexConcatRaw(t, []sk.Value{sk.NewList(boundary)}, nil)
	if err != nil {
		t.Fatalf("md5_hex_concat 10000: unexpected error: %v", err)
	}
	if v == sk.None {
		t.Error("md5_hex_concat 10000 valid: want hex, got None")
	}

	// Extra kwargs → None (total, not an UnpackArgs error).
	v, err = callMD5HexConcatRaw(t,
		[]sk.Value{sk.NewList([]sk.Value{sk.String(single)})},
		[]sk.Tuple{{sk.String("extra"), sk.String("1")}})
	if err != nil {
		t.Errorf("md5_hex_concat extra kwarg: want None with nil error, got error %v", err)
	} else if v != sk.None {
		t.Errorf("md5_hex_concat extra kwarg = %v, want None", v)
	}

	// Wrong positional arity → None (total, not an error).
	for _, args := range [][]sk.Value{
		nil,
		{sk.NewList([]sk.Value{sk.String(single)}), sk.NewList(nil)},
	} {
		v, err := callMD5HexConcatRaw(t, args, nil)
		if err != nil {
			t.Errorf("md5_hex_concat arity %d: want None with nil error, got error %v", len(args), err)
		} else if v != sk.None {
			t.Errorf("md5_hex_concat arity %d = %v, want None", len(args), v)
		}
	}
}
