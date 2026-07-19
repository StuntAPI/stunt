package manifest

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTLSFalseInManifestParsesAsExplicitFalse verifies that tls: false in
// the manifest is parsed as an explicit *bool(false), not the Go zero-value
// which is indistinguishable from "unset".
func TestTLSFalseInManifestParsesAsExplicitFalse(t *testing.T) {
	data := []byte(`version: 1
network:
  mode: subdomain
  tls: false
services:
  example:
    rules:
      - match: { path: / }
        respond: { status: 200 }
`)
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Network.TLS == nil {
		t.Fatal("TLS should be non-nil when explicitly set to false")
	}
	if *m.Network.TLS {
		t.Error("TLS should be false when manifest says tls: false")
	}
}

// TestTLSOmittedInManifestParsesAsNil verifies that when tls is omitted,
// TLS is nil (unset), so Defaults() can set the default.
func TestTLSOmittedInManifestParsesAsNil(t *testing.T) {
	data := []byte(`version: 1
network:
  mode: subdomain
services:
  example:
    rules:
      - match: { path: / }
        respond: { status: 200 }
`)
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Network.TLS != nil {
		t.Errorf("TLS should be nil when omitted, got %v", *m.Network.TLS)
	}
}

// TestTLSTrueInManifestParsesAsExplicitTrue verifies that tls: true is
// parsed as an explicit *bool(true).
func TestTLSTrueInManifestParsesAsExplicitTrue(t *testing.T) {
	data := []byte(`version: 1
network:
  mode: subdomain
  tls: true
services:
  example:
    rules:
      - match: { path: / }
        respond: { status: 200 }
`)
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Network.TLS == nil {
		t.Fatal("TLS should be non-nil when explicitly set to true")
	}
	if !*m.Network.TLS {
		t.Error("TLS should be true when manifest says tls: true")
	}
}

// TestNetworkDefaultsSetsTLSTrueForSubdomain verifies that Defaults()
// sets TLS to true when it's nil in subdomain mode.
func TestNetworkDefaultsSetsTLSTrueForSubdomain(t *testing.T) {
	n := Network{Mode: "subdomain"}
	n.Defaults()
	if n.TLS == nil {
		t.Fatal("Defaults() should set TLS for subdomain mode")
	}
	if !*n.TLS {
		t.Error("Defaults() should set TLS to true for subdomain mode")
	}
}

// TestNetworkDefaultsPreservesExplicitTLSFalse verifies that Defaults()
// does NOT override an explicitly-set tls: false.
func TestNetworkDefaultsPreservesExplicitTLSFalse(t *testing.T) {
	f := false
	n := Network{Mode: "subdomain", TLS: &f}
	n.Defaults()
	if n.TLS == nil || *n.TLS {
		t.Error("Defaults() should not override explicit tls: false")
	}
}

// TestNetworkDefaultsPreservesExplicitTLSTrue verifies that Defaults()
// does not override an explicitly-set tls: true.
func TestNetworkDefaultsPreservesExplicitTLSTrue(t *testing.T) {
	tr := true
	n := Network{Mode: "subdomain", TLS: &tr}
	n.Defaults()
	if n.TLS == nil || !*n.TLS {
		t.Error("Defaults() should not override explicit tls: true")
	}
}

// TestResolveTLS verifies the resolveTLS helper:
// - after Defaults(), TLS is never nil
// - --no-tls flag forces false
// - without the flag, the manifest value is used.
func TestResolveTLS(t *testing.T) {
	// manifest says tls: false → useTLS should be false
	n1 := Network{Mode: "subdomain"}
	n1.Defaults() // sets TLS to &true
	f := false
	n1.TLS = &f
	if ResolveTLS(&n1, false) {
		t.Error("tls:false + no flag → useTLS should be false")
	}
	if !ResolveTLS(&n1, true) {
		// --no-tls overrides even further — still false
		// actually --no-tls means "force no tls", so result should be false
	}
	if ResolveTLS(&n1, true) {
		t.Error("tls:false + --no-tls → useTLS should be false")
	}

	// manifest says tls: true (or omitted/defaulted)
	n2 := Network{Mode: "subdomain"}
	n2.Defaults()
	if !ResolveTLS(&n2, false) {
		t.Error("tls:true/omitted + no flag → useTLS should be true")
	}
	if ResolveTLS(&n2, true) {
		t.Error("tls:true/omitted + --no-tls → useTLS should be false")
	}
}
