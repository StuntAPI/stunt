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

// cryptoModule is the Starlark `crypto` module — the MAC and hash primitives an
// adapter needs to compute a webhook signature that a real receiver verifies.
//
// Charter (the scope line for future requests): symmetric MAC (HMAC) and
// cryptographic hash ONLY. No asymmetric signatures, encryption, KDFs, RNG,
// constant-time compare, or key management — none appear in webhook-signing
// schemes, and stunt already runs the same Go crypto in internal/primitives/
// identity for anything beyond. Keeping the surface to MAC+hash keeps it
// auditable.
//
// Starlark strings are UTF-8, so raw digest bytes cannot round-trip through a
// Starlark string. The MAC/hash functions therefore take an `encoding` kwarg
// ("hex" default | "base64") and return the final string directly.
var cryptoModule = &starlarkstruct.Module{
	Name: "crypto",
	Members: sk.StringDict{
		"hmac_sha256":   sk.NewBuiltin("crypto.hmac_sha256", hmacSHA256),
		"hmac_sha1":     sk.NewBuiltin("crypto.hmac_sha1", hmacSHA1),
		"sha256":        sk.NewBuiltin("crypto.sha256", sha256Hash),
		"base64_encode": sk.NewBuiltin("crypto.base64_encode", base64Encode),
		"base64_decode": sk.NewBuiltin("crypto.base64_decode", base64Decode),
	},
}

// encodeDigest hex- or base64-encodes a digest. Empty encoding defaults to hex.
func encodeDigest(sum []byte, encoding string) (string, error) {
	switch encoding {
	case "", "hex":
		return hex.EncodeToString(sum), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(sum), nil
	default:
		return "", fmt.Errorf("crypto: unknown encoding %q (want \"hex\" or \"base64\")", encoding)
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
