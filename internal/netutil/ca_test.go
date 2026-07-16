package netutil

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper: parse a PEM cert block.
func mustParseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("failed to decode PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

// helper: parse a PEM private key.
func mustParseKey(t *testing.T, pemBytes []byte) any {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("failed to decode PEM key block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	return key
}

func TestEnsureCA_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	// Both files should exist.
	if _, err := os.Stat(ca.CertPath); err != nil {
		t.Fatalf("cert file not created: %v", err)
	}
	if _, err := os.Stat(ca.KeyPath); err != nil {
		t.Fatalf("key file not created: %v", err)
	}

	// CertPEM should be populated.
	if ca.CertPEM == "" {
		t.Fatal("CertPEM is empty")
	}

	// The PEM should parse.
	block, _ := pem.Decode([]byte(ca.CertPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("CertPEM is not a valid CERTIFICATE PEM block, got type %q", block.Type)
	}
}

func TestEnsureCA_LoadsExisting(t *testing.T) {
	dir := t.TempDir()

	ca1, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("first EnsureCA: %v", err)
	}

	ca2, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("second EnsureCA: %v", err)
	}

	// Should be the same cert.
	if ca1.CertPEM != ca2.CertPEM {
		t.Fatal("EnsureCA regenerated the CA instead of loading existing")
	}
}

func TestEnsureCA_CertPaths(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	wantCert := filepath.Join(dir, "ca.pem")
	wantKey := filepath.Join(dir, "ca-key.pem")
	if ca.CertPath != wantCert {
		t.Errorf("CertPath = %q, want %q", ca.CertPath, wantCert)
	}
	if ca.KeyPath != wantKey {
		t.Errorf("KeyPath = %q, want %q", ca.KeyPath, wantKey)
	}
}

func TestCARootCertProperties(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	cert := mustParseCert(t, []byte(ca.CertPEM))

	if !cert.IsCA {
		t.Error("root cert does not have IsCA set")
	}
	if cert.SerialNumber == nil {
		t.Error("root cert has nil serial number")
	}
	// Basic constraints must be critical and CA=true.
	if cert.BasicConstraintsValid == false {
		t.Error("root cert BasicConstraintsValid is false")
	}
	// Key usage should include cert sign.
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("root cert KeyUsage missing CertSign")
	}
}

func TestCALeafCert(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	certPEM, keyPEM, err := ca.Leaf("myapp.localhost")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}

	if len(certPEM) == 0 {
		t.Fatal("leaf cert PEM is empty")
	}
	if len(keyPEM) == 0 {
		t.Fatal("leaf key PEM is empty")
	}

	leaf := mustParseCert(t, certPEM)
	rootCert := mustParseCert(t, []byte(ca.CertPEM))

	// Leaf must be signed by the CA (issuer == CA subject).
	if leaf.Issuer.String() != rootCert.Subject.String() {
		t.Errorf("leaf issuer %q != CA subject %q", leaf.Issuer, rootCert.Subject)
	}

	// Leaf should not be a CA.
	if leaf.IsCA {
		t.Error("leaf cert has IsCA set")
	}

	// Common name should be the host.
	if leaf.Subject.CommonName != "myapp.localhost" {
		t.Errorf("leaf CN = %q, want %q", leaf.Subject.CommonName, "myapp.localhost")
	}
}

func TestCALeafSANs(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	certPEM, _, err := ca.Leaf("myapp.localhost")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}

	leaf := mustParseCert(t, certPEM)

	// Collect DNS SANs into a set.
	dnsNames := make(map[string]bool)
	for _, name := range leaf.DNSNames {
		dnsNames[name] = true
	}

	// Must include the requested host plus default SANs.
	expectedDNS := []string{"myapp.localhost", "localhost", "*.localhost"}
	for _, name := range expectedDNS {
		if !dnsNames[name] {
			t.Errorf("leaf cert missing DNS SAN %q; DNSNames = %v", name, leaf.DNSNames)
		}
	}

	// Must include 127.0.0.1 IP SAN.
	found127 := false
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "127.0.0.1" {
			found127 = true
		}
	}
	if !found127 {
		t.Error("leaf cert missing IP SAN 127.0.0.1")
	}
}

func TestCALeafKeyIsECDSA(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	_, keyPEM, err := ca.Leaf("test.localhost")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}

	key := mustParseKey(t, keyPEM)
	if _, ok := key.(*ecdsa.PrivateKey); !ok {
		t.Errorf("leaf key is %T, want *ecdsa.PrivateKey", key)
	}
}

func TestCALeafCertAndKeyMatch(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	certPEM, keyPEM, err := ca.Leaf("test.localhost")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}

	leaf := mustParseCert(t, certPEM)
	key := mustParseKey(t, keyPEM)
	ecKey := key.(*ecdsa.PrivateKey)

	// The public key in the cert should match the private key.
	certPub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("leaf cert public key is %T, want *ecdsa.PublicKey", leaf.PublicKey)
	}
	if certPub.X.Cmp(ecKey.X) != 0 || certPub.Y.Cmp(ecKey.Y) != 0 {
		t.Error("leaf cert public key does not match leaf private key")
	}
}

func TestCARootKeyIsECDSA(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	keyData, err := os.ReadFile(ca.KeyPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	key := mustParseKey(t, keyData)
	if _, ok := key.(*ecdsa.PrivateKey); !ok {
		t.Errorf("root key is %T, want *ecdsa.PrivateKey", key)
	}
}

func TestCAKeyFilePermissions(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	info, err := os.Stat(ca.KeyPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// Key file should be readable only by owner (0600).
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestCAFilesArePEMEncoded(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	// Cert file.
	certData, _ := os.ReadFile(ca.CertPath)
	if !strings.HasPrefix(string(certData), "-----BEGIN CERTIFICATE-----") {
		t.Error("cert file does not start with PEM header")
	}

	// Key file.
	keyData, _ := os.ReadFile(ca.KeyPath)
	if !strings.HasPrefix(string(keyData), "-----BEGIN PRIVATE KEY-----") {
		t.Error("key file does not start with PEM header")
	}
}

func TestCALeafMultipleHosts(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}

	// Mint a cert for a bare host (no dots).
	certPEM1, _, err := ca.Leaf("foo")
	if err != nil {
		t.Fatalf("Leaf(foo): %v", err)
	}
	leaf1 := mustParseCert(t, certPEM1)
	if leaf1.Subject.CommonName != "foo" {
		t.Errorf("CN = %q, want %q", leaf1.Subject.CommonName, "foo")
	}

	// Mint for wildcard.
	certPEM2, _, err := ca.Leaf("*.localhost")
	if err != nil {
		t.Fatalf("Leaf(*.localhost): %v", err)
	}
	leaf2 := mustParseCert(t, certPEM2)
	if leaf2.Subject.CommonName != "*.localhost" {
		t.Errorf("CN = %q, want %q", leaf2.Subject.CommonName, "*.localhost")
	}
}

// --- Platform detection ---

func TestPlatformNonEmpty(t *testing.T) {
	p := Platform()
	if p == "" {
		t.Fatal("Platform() returned empty string")
	}
}

func TestPlatformKnownValue(t *testing.T) {
	p := Platform()
	known := map[string]bool{
		"darwin":      true,
		"windows":     true,
		"linux-debian": true,
		"linux-rhel":   true,
		"linux-other":  true,
	}
	if !known[p] {
		t.Errorf("Platform() = %q, not a recognized value", p)
	}
}

// --- TrustCommand tests (construct only, never run) ---

func TestTrustCommandDarwin(t *testing.T) {
	cmd, err := trustCommandFor("darwin", "/path/to/ca.pem")
	if err != nil {
		t.Fatalf("trustCommandFor: %v", err)
	}
	want := []string{
		"security", "add-trusted-cert",
		"-d",
		"-r", "trustRoot",
		"-k", "/Library/Keychains/System.keychain",
		"/path/to/ca.pem",
	}
	if !equalStrings(cmd.Args, want) {
		t.Errorf("darwin TrustCommand args = %v, want %v", cmd.Args, want)
	}
}

func TestTrustCommandLinuxDebian(t *testing.T) {
	cmd, err := trustCommandFor("linux-debian", "/path/to/ca.pem")
	if err != nil {
		t.Fatalf("trustCommandFor: %v", err)
	}
	// On Linux the command copies to the trust dir then runs the updater.
	// We assert the Args contain the right update command.
	if cmd.Args[0] != "sh" || cmd.Args[1] != "-c" {
		t.Fatalf("expected sh -c wrapper, got %v", cmd.Args)
	}
	script := cmd.Args[2]
	if !strings.Contains(script, "update-ca-certificates") {
		t.Errorf("debian trust script missing update-ca-certificates: %q", script)
	}
	if !strings.Contains(script, "/usr/local/share/ca-certificates") {
		t.Errorf("debian trust script missing target dir: %q", script)
	}
	if !strings.Contains(script, "/path/to/ca.pem") {
		t.Errorf("debian trust script missing ca path: %q", script)
	}
}

func TestTrustCommandLinuxRHEL(t *testing.T) {
	cmd, err := trustCommandFor("linux-rhel", "/path/to/ca.pem")
	if err != nil {
		t.Fatalf("trustCommandFor: %v", err)
	}
	if cmd.Args[0] != "sh" || cmd.Args[1] != "-c" {
		t.Fatalf("expected sh -c wrapper, got %v", cmd.Args)
	}
	script := cmd.Args[2]
	if !strings.Contains(script, "update-ca-trust") {
		t.Errorf("rhel trust script missing update-ca-trust: %q", script)
	}
	if !strings.Contains(script, "/etc/pki/ca-trust/source/anchors") {
		t.Errorf("rhel trust script missing target dir: %q", script)
	}
}

func TestTrustCommandWindows(t *testing.T) {
	cmd, err := trustCommandFor("windows", `C:\path\to\ca.pem`)
	if err != nil {
		t.Fatalf("trustCommandFor: %v", err)
	}
	want := []string{
		"certutil", "-addstore", "-f", "Root", `C:\path\to\ca.pem`,
	}
	if !equalStrings(cmd.Args, want) {
		t.Errorf("windows TrustCommand args = %v, want %v", cmd.Args, want)
	}
}

func TestTrustCommandLinuxOther(t *testing.T) {
	_, err := trustCommandFor("linux-other", "/path/to/ca.pem")
	if err == nil {
		t.Fatal("expected error for unsupported linux flavor")
	}
}

func TestTrustCommandUnknownPlatform(t *testing.T) {
	_, err := trustCommandFor("bsd", "/path/to/ca.pem")
	if err == nil {
		t.Fatal("expected error for unknown platform")
	}
}

// --- UntrustCommand tests ---

func TestUntrustCommandDarwin(t *testing.T) {
	cmd, err := untrustCommandFor("darwin", "/path/to/ca.pem")
	if err != nil {
		t.Fatalf("untrustCommandFor: %v", err)
	}
	if cmd.Args[0] != "security" {
		t.Fatalf("expected security command, got %q", cmd.Args[0])
	}
	// Should be deleting by the CA common name.
	if !contains(cmd.Args, "delete-certificate") {
		t.Errorf("darwin UntrustCommand missing delete-certificate: %v", cmd.Args)
	}
}

func TestUntrustCommandLinuxDebian(t *testing.T) {
	cmd, err := untrustCommandFor("linux-debian", "/path/to/ca.pem")
	if err != nil {
		t.Fatalf("untrustCommandFor: %v", err)
	}
	script := cmd.Args[2]
	if !strings.Contains(script, "update-ca-certificates") {
		t.Errorf("debian untrust script missing update-ca-certificates: %q", script)
	}
	if !strings.Contains(script, "rm ") {
		t.Errorf("debian untrust script missing rm: %q", script)
	}
}

func TestUntrustCommandWindows(t *testing.T) {
	cmd, err := untrustCommandFor("windows", `C:\path\to\ca.pem`)
	if err != nil {
		t.Fatalf("untrustCommandFor: %v", err)
	}
	if cmd.Args[0] != "certutil" {
		t.Fatalf("expected certutil, got %q", cmd.Args[0])
	}
	if !contains(cmd.Args, "-delstore") {
		t.Errorf("windows UntrustCommand missing -delstore: %v", cmd.Args)
	}
}

// --- Integration test (skipped by default) ---

func TestTrustCAIntegration(t *testing.T) {
	// This test would actually install the CA into the system trust store.
	// It requires real privilege (sudo on macOS/Linux, admin on Windows) and
	// should only run during a real `stunt setup`. Skip by default.
	t.Skip("integration test: requires real stunt setup / sudo to trust CA in system store")

	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	cmd, err := TrustCommand(ca.CertPath)
	if err != nil {
		t.Fatalf("TrustCommand: %v", err)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("trust command failed: %v", err)
	}
}

// --- helpers ---

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// --- I1: shell injection tests ---

func TestShellQuote(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"simple", "'simple'"},
		{"/path/to/file", "'/path/to/file'"},
		{"path with space", "'path with space'"},
		{"it's", "'it'\\''s'"},
	}
	for _, c := range cases {
		got := shellQuote(c.input)
		if got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestTrustCommandLinuxNoShellInjection(t *testing.T) {
	// A malicious CA path containing command substitution.
	evilPath := "$(touch /tmp/stunt-pwned)"

	// Debian
	cmd, err := trustCommandFor("linux-debian", evilPath)
	if err != nil {
		t.Fatal(err)
	}
	script := cmd.Args[2]
	// The path must be single-quoted so $() is literal, not executed.
	if !strings.Contains(script, "'"+evilPath+"'") {
		t.Errorf("debian trust script should single-quote the path: %q", script)
	}

	// RHEL
	cmd, err = trustCommandFor("linux-rhel", evilPath)
	if err != nil {
		t.Fatal(err)
	}
	script = cmd.Args[2]
	if !strings.Contains(script, "'"+evilPath+"'") {
		t.Errorf("rhel trust script should single-quote the path: %q", script)
	}
}

func TestTrustCommandLinuxBacktickNoShellInjection(t *testing.T) {
	evilPath := "`touch /tmp/stunt-pwned`"

	cmd, err := trustCommandFor("linux-debian", evilPath)
	if err != nil {
		t.Fatal(err)
	}
	script := cmd.Args[2]
	// The backtick path must be single-quoted.
	if !strings.Contains(script, "'"+evilPath+"'") {
		t.Errorf("debian trust script should single-quote backtick path: %q", script)
	}
}

func TestUntrustCommandLinuxNoShellInjection(t *testing.T) {
	evilPath := "$(touch /tmp/stunt-pwned)"

	cmd, err := untrustCommandFor("linux-debian", evilPath)
	if err != nil {
		t.Fatal(err)
	}
	script := cmd.Args[2]
	// The path may appear in the command but must be safely quoted.
	// In the untrust case, the target is a fixed path, so we verify
	// the script uses shellQuote on the fixed target.
	if !strings.Contains(script, "'") {
		t.Errorf("debian untrust script should use single quotes: %q", script)
	}
}

// --- I6: partial CA state tests ---

func TestEnsureCAErrorOnMissingKey(t *testing.T) {
	dir := t.TempDir()

	// First create a full CA.
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("first EnsureCA: %v", err)
	}

	// Delete the key file, leaving only the cert.
	if err := os.Remove(ca.KeyPath); err != nil {
		t.Fatalf("remove key: %v", err)
	}

	// EnsureCA should now error instead of silently regenerating.
	ca2, err := EnsureCA(dir)
	if err == nil {
		t.Fatal("EnsureCA should error when key is missing but cert exists")
	}
	if ca2 != nil {
		t.Fatal("EnsureCA should return nil CA on partial state")
	}

	// Verify the cert file still exists (not orphaned/overwritten).
	if _, err := os.Stat(ca.CertPath); err != nil {
		t.Error("cert file should still exist")
	}
}

func TestEnsureCAErrorOnMissingCert(t *testing.T) {
	dir := t.TempDir()

	// First create a full CA.
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatalf("first EnsureCA: %v", err)
	}

	// Delete the cert file, leaving only the key.
	if err := os.Remove(ca.CertPath); err != nil {
		t.Fatalf("remove cert: %v", err)
	}

	// EnsureCA should now error.
	_, err = EnsureCA(dir)
	if err == nil {
		t.Fatal("EnsureCA should error when cert is missing but key exists")
	}

	// Verify the key file still exists (not orphaned/overwritten).
	if _, err := os.Stat(ca.KeyPath); err != nil {
		t.Error("key file should still exist")
	}
}
