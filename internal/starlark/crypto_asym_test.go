package starlark

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"

	sk "go.starlark.net/starlark"
)

func testECDSAKeyPair(t *testing.T) (priv, pub string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	priv = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	pub = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return
}

func testRSAKeyPair(t *testing.T) (priv, pub string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privDER := x509.MarshalPKCS1PrivateKey(k)
	pubDER, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	priv = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER}))
	pub = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return
}

func testEd25519KeyPair(t *testing.T) (priv, pub string) {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	priv = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))
	pub = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	return
}

func callAsym(t *testing.T, name string, args sk.Tuple) sk.Value {
	t.Helper()
	fn, err := cryptoModule.Attr(name)
	if err != nil {
		t.Fatalf("crypto.%s not registered: %v", name, err)
	}
	res, err := sk.Call(new(sk.Thread), fn, args, nil)
	if err != nil {
		t.Fatalf("crypto.%s: %v", name, err)
	}
	return res
}

func callAsymErr(name string, args sk.Tuple) error {
	fn, _ := cryptoModule.Attr(name)
	_, err := sk.Call(new(sk.Thread), fn, args, nil)
	return err
}

func TestECDSAP256SignVerifyRoundTrip(t *testing.T) {
	priv, pub := testECDSAKeyPair(t)
	const data = "the quick brown fox"
	for _, enc := range []string{"hex", "base64", "base64url"} {
		sig := string(callAsym(t, "ecdsa_sign_p256", sk.Tuple{sk.String(priv), sk.String(data), sk.String(enc)}).(sk.String))
		if sig == "" {
			t.Fatalf("enc=%s: empty signature", enc)
		}
		ok := bool(callAsym(t, "ecdsa_verify_p256", sk.Tuple{sk.String(pub), sk.String(data), sk.String(sig), sk.String(enc)}).(sk.Bool))
		if !ok {
			t.Errorf("enc=%s: verify failed", enc)
		}
	}
}

func TestECDSAP256TamperFails(t *testing.T) {
	priv, pub := testECDSAKeyPair(t)
	sig := string(callAsym(t, "ecdsa_sign_p256", sk.Tuple{sk.String(priv), sk.String("original"), sk.String("hex")}).(sk.String))
	// Different data with the same signature must not verify.
	ok := bool(callAsym(t, "ecdsa_verify_p256", sk.Tuple{sk.String(pub), sk.String("tampered"), sk.String(sig), sk.String("hex")}).(sk.Bool))
	if ok {
		t.Error("verify succeeded on tampered data; want false")
	}
	// A truncated signature is not valid (64-byte r‖s) → false, not an error.
	short := string(callAsym(t, "ecdsa_sign_p256", sk.Tuple{sk.String(priv), sk.String("x"), sk.String("hex")}).(sk.String))[:10]
	ok = bool(callAsym(t, "ecdsa_verify_p256", sk.Tuple{sk.String(pub), sk.String("x"), sk.String(short), sk.String("hex")}).(sk.Bool))
	if ok {
		t.Error("verify succeeded on truncated signature; want false")
	}
}

func TestRSASignVerifyRoundTrip(t *testing.T) {
	priv, pub := testRSAKeyPair(t)
	const data = "the quick brown fox"
	for _, enc := range []string{"hex", "base64", "base64url"} {
		sig := string(callAsym(t, "rsa_sign", sk.Tuple{sk.String(priv), sk.String(data), sk.String(enc)}).(sk.String))
		ok := bool(callAsym(t, "rsa_verify", sk.Tuple{sk.String(pub), sk.String(data), sk.String(sig), sk.String(enc)}).(sk.Bool))
		if !ok {
			t.Errorf("enc=%s: verify failed", enc)
		}
	}
	// RS256 (PKCS#1 v1.5) is deterministic: same input → same signature.
	a := string(callAsym(t, "rsa_sign", sk.Tuple{sk.String(priv), sk.String(data), sk.String("hex")}).(sk.String))
	b := string(callAsym(t, "rsa_sign", sk.Tuple{sk.String(priv), sk.String(data), sk.String("hex")}).(sk.String))
	if a != b {
		t.Error("rsa_sign not deterministic; RS256 (PKCS#1 v1.5) should be")
	}
}

func TestRSATamperFails(t *testing.T) {
	priv, pub := testRSAKeyPair(t)
	sig := string(callAsym(t, "rsa_sign", sk.Tuple{sk.String(priv), sk.String("original"), sk.String("hex")}).(sk.String))
	ok := bool(callAsym(t, "rsa_verify", sk.Tuple{sk.String(pub), sk.String("tampered"), sk.String(sig), sk.String("hex")}).(sk.Bool))
	if ok {
		t.Error("verify succeeded on tampered data; want false")
	}
}

func TestAsymWrongKeyTypeErrors(t *testing.T) {
	ecPriv, _ := testECDSAKeyPair(t)
	rsaPriv, _ := testRSAKeyPair(t)
	// RSA key fed to ECDSA signer → error, not a panic.
	if err := callAsymErr("ecdsa_sign_p256", sk.Tuple{sk.String(rsaPriv), sk.String("x"), sk.String("hex")}); err == nil {
		t.Error("ecdsa_sign_p256 with RSA key: want error, got nil")
	}
	// ECDSA key fed to RSA signer → error.
	if err := callAsymErr("rsa_sign", sk.Tuple{sk.String(ecPriv), sk.String("x"), sk.String("hex")}); err == nil {
		t.Error("rsa_sign with ECDSA key: want error, got nil")
	}
	// Garbage PEM → error.
	if err := callAsymErr("rsa_sign", sk.Tuple{sk.String("not a pem"), sk.String("x"), sk.String("hex")}); err == nil {
		t.Error("rsa_sign with garbage: want error, got nil")
	}
}

func TestEd25519SignVerifyRoundTrip(t *testing.T) {
	priv, pub := testEd25519KeyPair(t)
	const data = "the quick brown fox"
	for _, enc := range []string{"hex", "base64", "base64url"} {
		sig := string(callAsym(t, "ed25519_sign", sk.Tuple{sk.String(priv), sk.String(data), sk.String(enc)}).(sk.String))
		if sig == "" {
			t.Fatalf("enc=%s: empty signature", enc)
		}
		ok := bool(callAsym(t, "ed25519_verify", sk.Tuple{sk.String(pub), sk.String(data), sk.String(sig), sk.String(enc)}).(sk.Bool))
		if !ok {
			t.Errorf("enc=%s: verify failed", enc)
		}
	}
}

func TestEd25519TamperFails(t *testing.T) {
	priv, pub := testEd25519KeyPair(t)
	sig := string(callAsym(t, "ed25519_sign", sk.Tuple{sk.String(priv), sk.String("original"), sk.String("hex")}).(sk.String))
	ok := bool(callAsym(t, "ed25519_verify", sk.Tuple{sk.String(pub), sk.String("tampered"), sk.String(sig), sk.String("hex")}).(sk.Bool))
	if ok {
		t.Error("verify succeeded on tampered data; want false")
	}
	// Ed25519 is deterministic: same input → same signature.
	a := string(callAsym(t, "ed25519_sign", sk.Tuple{sk.String(priv), sk.String("original"), sk.String("hex")}).(sk.String))
	b := string(callAsym(t, "ed25519_sign", sk.Tuple{sk.String(priv), sk.String("original"), sk.String("hex")}).(sk.String))
	if a != b {
		t.Error("ed25519_sign not deterministic")
	}
}

func TestEd25519WrongKeyTypeErrors(t *testing.T) {
	rsaPriv, _ := testRSAKeyPair(t)
	if err := callAsymErr("ed25519_sign", sk.Tuple{sk.String(rsaPriv), sk.String("x"), sk.String("hex")}); err == nil {
		t.Error("ed25519_sign with RSA key: want error, got nil")
	}
}

// TestRSAPublicJWK verifies rsa_public_jwk returns {kty,n,e} whose base64url n/e
// reconstruct the original public key's modulus and exponent.
func TestRSAPublicJWK(t *testing.T) {
	_, pubPEM := testRSAKeyPair(t)
	fn, err := cryptoModule.Attr("rsa_public_jwk")
	if err != nil {
		t.Fatal(err)
	}
	res, err := sk.Call(new(sk.Thread), fn, sk.Tuple{sk.String(pubPEM)}, nil)
	if err != nil {
		t.Fatalf("rsa_public_jwk: %v", err)
	}
	d, ok := res.(*sk.Dict)
	if !ok {
		t.Fatalf("got %T, want dict", res)
	}
	if v, _, _ := d.Get(sk.String("kty")); string(v.(sk.String)) != "RSA" {
		t.Errorf("kty = %v, want RSA", v)
	}
	nVal, _, _ := d.Get(sk.String("n"))
	eVal, _, _ := d.Get(sk.String("e"))
	nBytes, err := base64.RawURLEncoding.DecodeString(string(nVal.(sk.String)))
	if err != nil {
		t.Fatalf("decode n: %v", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(string(eVal.(sk.String)))
	if err != nil {
		t.Fatalf("decode e: %v", err)
	}
	if new(big.Int).SetBytes(nBytes).BitLen() == 0 {
		t.Error("n is zero")
	}
	if eInt := int(new(big.Int).SetBytes(eBytes).Int64()); eInt != 65537 {
		t.Errorf("e = %d, want 65537", eInt)
	}
}
