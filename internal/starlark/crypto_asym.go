package starlark

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"

	sk "go.starlark.net/starlark"
)

// parsePrivateKeyPEM decodes a PEM-encoded private key (PKCS#8, PKCS#1 RSA, or
// SEC1 ECDSA). The concrete type is an RSA or ECDSA private key.
func parsePrivateKeyPEM(pemStr string) (crypto.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("crypto: private key has no PEM block")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("crypto: unrecognized private key PEM (want PKCS#8, PKCS#1 RSA, or SEC1 ECDSA)")
}

// parsePublicKeyPEM decodes a PEM-encoded public key (PKIX: RSA or ECDSA).
func parsePublicKeyPEM(pemStr string) (crypto.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("crypto: public key has no PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("crypto: unrecognized public key PEM: %w", err)
	}
	return pub, nil
}

// leftPad pads a big-endian byte slice to size, prepending zeros. ECDSA P-256
// r/s values are variable-length; ES256 needs each fixed at 32 bytes.
func leftPad(b []byte, size int) []byte {
	if len(b) >= size {
		return b[len(b)-size:]
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}

// ecdsaSignP256 signs data with an ECDSA P-256 private key, returning the raw
// r‖s signature (64 bytes) encoded per `encoding` (hex default / base64 /
// base64url). The nonce uses crypto/rand; the resulting signature still verifies
// deterministically against the public key.
func ecdsaSignP256(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var privPEM, data, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "private_key", &privPEM, "data", &data, "encoding?", &encoding); err != nil {
		return nil, err
	}
	k, err := parsePrivateKeyPEM(privPEM)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(*ecdsa.PrivateKey)
	if !ok || priv.Curve.Params().BitSize != 256 {
		return nil, fmt.Errorf("crypto.ecdsa_sign_p256: key is not an ECDSA P-256 private key")
	}
	h := sha256.Sum256([]byte(data))
	r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
	if err != nil {
		return nil, fmt.Errorf("crypto.ecdsa_sign_p256: %w", err)
	}
	sig := append(leftPad(r.Bytes(), 32), leftPad(s.Bytes(), 32)...)
	out, err := encodeDigest(sig, encoding)
	if err != nil {
		return nil, err
	}
	return sk.String(out), nil
}

// ecdsaVerifyP256 verifies a raw r‖s ECDSA P-256 signature against the public
// key. Returns True on a valid signature, False otherwise. A malformed key or
// signature is a caller error.
func ecdsaVerifyP256(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var pubPEM, data, sigStr, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "public_key", &pubPEM, "data", &data, "signature", &sigStr, "encoding?", &encoding); err != nil {
		return nil, err
	}
	k, err := parsePublicKeyPEM(pubPEM)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok || pub.Curve.Params().BitSize != 256 {
		return nil, fmt.Errorf("crypto.ecdsa_verify_p256: key is not an ECDSA P-256 public key")
	}
	sig, err := decodeDigest(sigStr, encoding)
	if err != nil {
		return nil, fmt.Errorf("crypto.ecdsa_verify_p256: signature: %w", err)
	}
	if len(sig) != 64 {
		return sk.Bool(false), nil
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(data))
	return sk.Bool(ecdsa.Verify(pub, h[:], r, s)), nil
}

// rsaSign signs data with RSA-SHA256 (PKCS#1 v1.5), returning the signature
// encoded per `encoding` (hex default / base64 / base64url). PKCS#1 v1.5 is
// deterministic, so RS256 JWTs are reproducible.
func rsaSign(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var privPEM, data, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "private_key", &privPEM, "data", &data, "encoding?", &encoding); err != nil {
		return nil, err
	}
	k, err := parsePrivateKeyPEM(privPEM)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("crypto.rsa_sign: key is not an RSA private key")
	}
	h := sha256.Sum256([]byte(data))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	if err != nil {
		return nil, fmt.Errorf("crypto.rsa_sign: %w", err)
	}
	out, err := encodeDigest(sig, encoding)
	if err != nil {
		return nil, err
	}
	return sk.String(out), nil
}

// rsaVerify verifies an RSA-SHA256 (PKCS#1 v1.5) signature against the public
// key. Returns True on a valid signature, False otherwise.
func rsaVerify(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var pubPEM, data, sigStr, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "public_key", &pubPEM, "data", &data, "signature", &sigStr, "encoding?", &encoding); err != nil {
		return nil, err
	}
	k, err := parsePublicKeyPEM(pubPEM)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("crypto.rsa_verify: key is not an RSA public key")
	}
	sig, err := decodeDigest(sigStr, encoding)
	if err != nil {
		return nil, fmt.Errorf("crypto.rsa_verify: signature: %w", err)
	}
	h := sha256.Sum256([]byte(data))
	return sk.Bool(rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig) == nil), nil
}

// rsaPublicJWK returns the JWK parameters of an RSA public key as a dict
// {kty, n, e} (base64url, no padding) for serving a JWKS endpoint the way JWT
// issuers (Entra ID, Cognito, Firebase, Google, Apple) do.
func rsaPublicJWK(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var pubPEM string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "public_key", &pubPEM); err != nil {
		return nil, err
	}
	k, err := parsePublicKeyPEM(pubPEM)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("crypto.rsa_public_jwk: key is not an RSA public key")
	}
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	d := sk.NewDict(3)
	d.SetKey(sk.String("kty"), sk.String("RSA"))
	d.SetKey(sk.String("n"), sk.String(base64.RawURLEncoding.EncodeToString(pub.N.Bytes())))
	d.SetKey(sk.String("e"), sk.String(base64.RawURLEncoding.EncodeToString(eBytes)))
	return d, nil
}

// ed25519Sign signs data with an Ed25519 private key (signs the message
// directly — Ed25519 does not pre-hash), returning the 64-byte signature
// encoded per `encoding` (hex default / base64 / base64url).
func ed25519Sign(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var privPEM, data, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "private_key", &privPEM, "data", &data, "encoding?", &encoding); err != nil {
		return nil, err
	}
	k, err := parsePrivateKeyPEM(privPEM)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("crypto.ed25519_sign: key is not an Ed25519 private key")
	}
	sig := ed25519.Sign(priv, []byte(data))
	out, err := encodeDigest(sig, encoding)
	if err != nil {
		return nil, err
	}
	return sk.String(out), nil
}

// ed25519Verify verifies an Ed25519 signature against the public key. Returns
// True on a valid signature, False otherwise.
func ed25519Verify(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var pubPEM, data, sigStr, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "public_key", &pubPEM, "data", &data, "signature", &sigStr, "encoding?", &encoding); err != nil {
		return nil, err
	}
	k, err := parsePublicKeyPEM(pubPEM)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("crypto.ed25519_verify: key is not an Ed25519 public key")
	}
	sig, err := decodeDigest(sigStr, encoding)
	if err != nil {
		return nil, fmt.Errorf("crypto.ed25519_verify: signature: %w", err)
	}
	return sk.Bool(ed25519.Verify(pub, []byte(data), sig)), nil
}
