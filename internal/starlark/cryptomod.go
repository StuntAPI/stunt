package starlark

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	sk "go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// cryptoModule is the Starlark `crypto` module — the primitives an adapter
// needs to compute a signature that a real receiver verifies: symmetric MAC
// (HMAC), cryptographic hash, and the asymmetric signatures (ECDSA P-256,
// RSA) that JWT issuers and signed webhooks use.
//
// Charter (the scope line for future requests): MAC, hash, and asymmetric
// signature (sign/verify) ONLY. No encryption, KDFs, RNG, constant-time
// compare, or key management/generation. Keys arrive as PEM strings the caller
// supplies (adapters ship a fixed, documented keypair for determinism);
// signatures are returned encoded so they round-trip through a Starlark string.
//
// Starlark strings are UTF-8, so raw digest/signature bytes cannot round-trip
// through a Starlark string. The functions therefore take an `encoding` kwarg
// ("hex" default | "base64" | "base64url") and return the final string directly.
var cryptoModule = &starlarkstruct.Module{
	Name: "crypto",
	Members: sk.StringDict{
		"hmac_sha256":       sk.NewBuiltin("crypto.hmac_sha256", hmacSHA256),
		"hmac_sha1":         sk.NewBuiltin("crypto.hmac_sha1", hmacSHA1),
		"sha256":            sk.NewBuiltin("crypto.sha256", sha256Hash),
		"base64_encode":     sk.NewBuiltin("crypto.base64_encode", base64Encode),
		"base64_decode":     sk.NewBuiltin("crypto.base64_decode", base64Decode),
		"base64url_encode":  sk.NewBuiltin("crypto.base64url_encode", base64urlEncode),
		"ecdsa_sign_p256":   sk.NewBuiltin("crypto.ecdsa_sign_p256", ecdsaSignP256),
		"ecdsa_verify_p256": sk.NewBuiltin("crypto.ecdsa_verify_p256", ecdsaVerifyP256),
		"rsa_sign":          sk.NewBuiltin("crypto.rsa_sign", rsaSign),
		"rsa_verify":        sk.NewBuiltin("crypto.rsa_verify", rsaVerify),
		"rsa_public_jwk":    sk.NewBuiltin("crypto.rsa_public_jwk", rsaPublicJWK),
	},
}

// encodeDigest hex- or base64-encodes a digest. Empty encoding defaults to hex.
func encodeDigest(sum []byte, encoding string) (string, error) {
	switch encoding {
	case "", "hex":
		return hex.EncodeToString(sum), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(sum), nil
	case "base64url":
		return base64.RawURLEncoding.EncodeToString(sum), nil
	default:
		return "", fmt.Errorf("crypto: unknown encoding %q (want \"hex\", \"base64\", or \"base64url\")", encoding)
	}
}

// decodeDigest is the inverse of encodeDigest: it decodes a hex/base64/base64url
// string into raw bytes.
func decodeDigest(s, encoding string) ([]byte, error) {
	switch encoding {
	case "", "hex":
		return hex.DecodeString(s)
	case "base64":
		return base64.StdEncoding.DecodeString(s)
	case "base64url":
		return base64.RawURLEncoding.DecodeString(s)
	default:
		return nil, fmt.Errorf("crypto: unknown encoding %q (want \"hex\", \"base64\", or \"base64url\")", encoding)
	}
}

func hmacSHA256(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var key, data, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "key", &key, "data", &data, "encoding?", &encoding); err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(data))
	out, err := encodeDigest(mac.Sum(nil), encoding)
	if err != nil {
		return nil, err
	}
	return sk.String(out), nil
}

func hmacSHA1(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var key, data, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "key", &key, "data", &data, "encoding?", &encoding); err != nil {
		return nil, err
	}
	mac := hmac.New(sha1.New, []byte(key))
	mac.Write([]byte(data))
	out, err := encodeDigest(mac.Sum(nil), encoding)
	if err != nil {
		return nil, err
	}
	return sk.String(out), nil
}

func sha256Hash(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var data, encoding string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "data", &data, "encoding?", &encoding); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(data))
	out, err := encodeDigest(sum[:], encoding)
	if err != nil {
		return nil, err
	}
	return sk.String(out), nil
}

func base64Encode(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var data string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "data", &data); err != nil {
		return nil, err
	}
	return sk.String(base64.StdEncoding.EncodeToString([]byte(data))), nil
}

func base64Decode(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var s string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "data", &s); err != nil {
		return nil, err
	}
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("crypto.base64_decode: %w", err)
	}
	return sk.String(string(out)), nil
}

func base64urlEncode(_ *sk.Thread, b *sk.Builtin, args sk.Tuple, kwargs []sk.Tuple) (sk.Value, error) {
	var data string
	if err := sk.UnpackArgs(b.Name(), args, kwargs, "data", &data); err != nil {
		return nil, err
	}
	return sk.String(base64.RawURLEncoding.EncodeToString([]byte(data))), nil
}
