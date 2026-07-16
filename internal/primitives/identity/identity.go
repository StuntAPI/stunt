// Package identity provides a local "yes" auth issuer that mints and
// validates opaque HMAC-SHA256 tokens with simulated scopes. This lets
// adapters simulate authentication without a real OAuth server.
//
// Token format: base64url(payload).base64url(signature)
//
// The payload is a JSON object {sub, scopes, exp}. The signature is the
// HMAC-SHA256 of the base64url-encoded payload using the issuer's secret.
// Validation re-computes the HMAC (constant-time compare) and checks expiry.
package identity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors returned by Validate.
var (
	// ErrInvalidToken is returned when the token is malformed, the signature
	// does not verify, or the payload cannot be decoded.
	ErrInvalidToken = errors.New("identity: invalid token")

	// ErrExpiredToken is returned when the token signature is valid but the
	// expiry has passed.
	ErrExpiredToken = errors.New("identity: token expired")
)

// Claims holds the decoded token payload.
type Claims struct {
	Subject   string    `json:"sub"`
	Scopes    []string  `json:"scopes,omitempty"`
	ExpiresAt time.Time `json:"exp"`
}

// payload is the on-the-wire JSON representation of a token's payload.
// The expiry is stored as a Unix-nanosecond timestamp for sub-second
// precision (needed for short TTLs in tests and simulators).
type payload struct {
	Sub    string   `json:"sub"`
	Scopes []string `json:"scopes,omitempty"`
	Exp    int64    `json:"exp"`
}

// Issuer mints and validates opaque HMAC-signed tokens.
type Issuer struct {
	secret []byte
}

// NewIssuer creates an Issuer that signs tokens with the given secret.
// The secret is copied so the caller may safely mutate the original slice.
func NewIssuer(secret []byte) *Issuer {
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &Issuer{secret: cp}
}

// Mint creates a signed token for subject with the given scopes, valid for ttl.
// Returns the opaque token string.
func (i *Issuer) Mint(subject string, scopes []string, ttl time.Duration) (string, error) {
	exp := time.Now().Add(ttl)

	p := payload{
		Sub:    subject,
		Scopes: scopes,
		Exp:    exp.UnixNano(),
	}
	payloadJSON, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("identity: marshal payload: %w", err)
	}

	encoded := base64.RawURLEncoding.EncodeToString(payloadJSON)
	sig := i.computeSig(encoded)

	return encoded + "." + sig, nil
}

// Validate verifies the token's signature and checks expiry. On success
// returns the decoded Claims. Returns ErrInvalidToken for malformed or
// bad-signature tokens, and ErrExpiredToken for valid-but-expired tokens.
func (i *Issuer) Validate(token string) (*Claims, error) {
	parts := splitToken(token)
	if parts == nil {
		return nil, ErrInvalidToken
	}
	encoded, sig := parts[0], parts[1]

	// Verify signature before decoding payload (constant-time compare).
	expected := i.computeSig(encoded)
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return nil, ErrInvalidToken
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrInvalidToken
	}

	var p payload
	if err := json.Unmarshal(payloadJSON, &p); err != nil {
		return nil, ErrInvalidToken
	}

	claims := &Claims{
		Subject:   p.Sub,
		Scopes:    p.Scopes,
		ExpiresAt: time.Unix(0, p.Exp),
	}

	if time.Now().After(claims.ExpiresAt) {
		return nil, ErrExpiredToken
	}

	return claims, nil
}

// computeSig returns the base64url-encoded HMAC-SHA256 of encoded using
// the issuer's secret.
func (i *Issuer) computeSig(encoded string) string {
	mac := hmac.New(sha256.New, i.secret)
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// splitToken splits a token into its two dot-separated parts. Returns nil
// if the token does not have exactly one '.' separator.
func splitToken(token string) []string {
	dot := -1
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			if dot != -1 {
				return nil // more than one dot
			}
			dot = i
		}
	}
	if dot <= 0 || dot == len(token)-1 {
		return nil
	}
	return []string{token[:dot], token[dot+1:]}
}

// HasScope reports whether claims grant the given scope. Returns false if
// claims is nil or the scope is absent.
func HasScope(claims *Claims, scope string) bool {
	if claims == nil {
		return false
	}
	for _, s := range claims.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}
