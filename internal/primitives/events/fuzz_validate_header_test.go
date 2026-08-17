package events

import (
	"strings"
	"testing"
)

// FuzzValidateHeader drives the outgoing-webhook header validator with
// arbitrary keys and values. Invariants: no panic; anything accepted must
// be safe to hand to http.Header.Set — no CR or LF anywhere, or the
// delivery becomes a header-smuggling vector.
func FuzzValidateHeader(f *testing.F) {
	for _, s := range [][2]string{
		{"X-Test", "v"},
		{"X-Bad", "v\r\nX-Inject: yes"},
		{"X-Bad\r\nInjected: yes", "v"},
		{"Host", "example.com"},
		{"Content-Length", "5"},
		{"", ""},
		{"x-signature-ed25519", "3×q=base64=="},
		{"🦀", "🦀"},
		{"X-Ok", "line1\rline2"},
		{"X-Ok", "line1\nline2"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, key, val string) {
		if err := validateHeader(key, val); err == nil {
			if strings.ContainsAny(key, "\r\n") || strings.ContainsAny(val, "\r\n") {
				t.Fatalf("validateHeader accepted CRLF: key=%q val=%q", key, val)
			}
		}
	})
}
