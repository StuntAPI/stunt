package identity

import (
	"errors"
	"strings"
	"testing"
	"time"

	"stuntapi.com/stunt/internal/primitives/clock"
)

func newTestIssuer(t *testing.T) *Issuer {
	t.Helper()
	return NewIssuer([]byte("test-secret-key"))
}

func TestMintValidateRoundtrip(t *testing.T) {
	iss := newTestIssuer(t)

	token, err := iss.Mint("user-42", []string{"read", "write"}, time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if token == "" {
		t.Fatal("Mint returned empty token")
	}
	if !strings.Contains(token, ".") {
		t.Fatalf("token %q should contain a '.' separator", token)
	}

	claims, err := iss.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Subject != "user-42" {
		t.Fatalf("Subject = %q, want user-42", claims.Subject)
	}
	if len(claims.Scopes) != 2 {
		t.Fatalf("len(Scopes) = %d, want 2", len(claims.Scopes))
	}
	if claims.Scopes[0] != "read" || claims.Scopes[1] != "write" {
		t.Fatalf("Scopes = %v, want [read write]", claims.Scopes)
	}

	// ExpiresAt should be ~1 hour from now.
	if claims.ExpiresAt.IsZero() {
		t.Fatal("ExpiresAt is zero")
	}
}

func TestMintDifferentSecrets(t *testing.T) {
	iss1 := NewIssuer([]byte("secret-one"))
	iss2 := NewIssuer([]byte("secret-two"))

	token, _ := iss1.Mint("alice", []string{"read"}, time.Hour)

	// Token minted by iss1 should not validate with iss2.
	_, err := iss2.Validate(token)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Validate with different secret: err = %v, want ErrInvalidToken", err)
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	iss := newTestIssuer(t)

	token, _ := iss.Mint("user", []string{"read"}, time.Hour)

	// Tamper with the payload portion.
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("token %q should have 2 parts", token)
	}
	tampered := parts[0] + "x." + parts[1]

	_, err := iss.Validate(tampered)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Validate tampered: err = %v, want ErrInvalidToken", err)
	}

	// Tamper with the signature portion.
	tamperedSig := parts[0] + "." + parts[1] + "x"
	_, err = iss.Validate(tamperedSig)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Validate tampered sig: err = %v, want ErrInvalidToken", err)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	iss := newTestIssuer(t)

	token, _ := iss.Mint("user", []string{"read"}, -1*time.Hour)

	_, err := iss.Validate(token)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("Validate expired: err = %v, want ErrExpiredToken", err)
	}
}

func TestMalformedTokenRejected(t *testing.T) {
	iss := newTestIssuer(t)

	for _, bad := range []string{
		"",
		"noseparator",
		"only-one-part",
		"a.b.c",
		"...",
	} {
		_, err := iss.Validate(bad)
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Validate(%q): err = %v, want ErrInvalidToken", bad, err)
		}
	}
}

func TestHasScope(t *testing.T) {
	claims := &Claims{Scopes: []string{"read", "write"}}

	if !HasScope(claims, "read") {
		t.Error("HasScope(read) = false, want true")
	}
	if !HasScope(claims, "write") {
		t.Error("HasScope(write) = false, want true")
	}
	if HasScope(claims, "admin") {
		t.Error("HasScope(admin) = true, want false")
	}
}

func TestHasScopeEmpty(t *testing.T) {
	claims := &Claims{Scopes: nil}
	if HasScope(claims, "read") {
		t.Error("HasScope(read) on nil scopes = true, want false")
	}
}

func TestHasScopeNilClaims(t *testing.T) {
	if HasScope(nil, "read") {
		t.Error("HasScope(nil, read) = true, want false")
	}
}

func TestTTLExpiry(t *testing.T) {
	iss := newTestIssuer(t)

	// Token with 1ms TTL should be valid now but expired after sleeping.
	token, _ := iss.Mint("user", []string{"read"}, 50*time.Millisecond)

	// Should be valid immediately.
	claims, err := iss.Validate(token)
	if err != nil {
		t.Fatalf("Validate immediately: %v", err)
	}
	if claims.Subject != "user" {
		t.Fatalf("Subject = %q, want user", claims.Subject)
	}

	// After expiry it should be rejected.
	time.Sleep(100 * time.Millisecond)
	_, err = iss.Validate(token)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("Validate after expiry: err = %v, want ErrExpiredToken", err)
	}
}

func TestEmptyScopes(t *testing.T) {
	iss := newTestIssuer(t)

	token, _ := iss.Mint("guest", nil, time.Hour)

	claims, err := iss.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(claims.Scopes) != 0 {
		t.Fatalf("len(Scopes) = %d, want 0", len(claims.Scopes))
	}
}

// TestIssuerWithInjectableClock proves mint/validate timing follows the
// injected clock, so expiry is deterministic for record/replay — no real
// time.Sleep, no dependence on time.Now.
func TestIssuerWithInjectableClock(t *testing.T) {
	start := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	vc := clock.NewVirtualClock(start)
	iss := NewIssuerWithClock([]byte("vc-secret"), vc)

	token, err := iss.Mint("u", []string{"read"}, time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// In-window: valid at mint time and just before the TTL elapses.
	if _, err := iss.Validate(token); err != nil {
		t.Fatalf("Validate at start: %v", err)
	}
	vc.Advance(59 * time.Minute)
	if _, err := iss.Validate(token); err != nil {
		t.Fatalf("Validate at 59m: %v", err)
	}

	// Past the TTL on the SAME clock → expired, deterministically.
	vc.Advance(2 * time.Minute)
	_, err = iss.Validate(token)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("Validate after TTL on virtual clock: err = %v, want ErrExpiredToken", err)
	}

	// A fresh token minted after the advance is valid again (exp = now+1h).
	token2, _ := iss.Mint("u", []string{"read"}, time.Hour)
	if _, err := iss.Validate(token2); err != nil {
		t.Fatalf("Validate fresh token after advance: %v", err)
	}

	// Determinism: two independent issuers whose virtual clocks agree produce
	// byte-identical tokens for the same inputs — the property record/replay
	// relies on. A time.Now() implementation would (almost) never match here.
	a := NewIssuerWithClock([]byte("det"), clock.NewVirtualClock(start))
	b := NewIssuerWithClock([]byte("det"), clock.NewVirtualClock(start))
	ta, _ := a.Mint("sub", []string{"read", "write"}, 2*time.Hour)
	tb, _ := b.Mint("sub", []string{"read", "write"}, 2*time.Hour)
	if ta != tb {
		t.Fatalf("tokens differ across virtual-clock instances — not deterministic:\n a=%s\n b=%s", ta, tb)
	}
}
