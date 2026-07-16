// Package netutil provides networking helpers for stunt: local CA
// generation, platform trust-store commands, and /etc/hosts management.
//
// # Host safety
//
// The trust and hosts functions in this package CONSTRUCT commands but do
// not execute them. The real invocations (trust-store mutation, /etc/hosts
// edits) happen only when the user runs `stunt setup` or `stunt trust`.
// All unit tests use temp directories and fakes — they never touch the real
// system trust store, /etc/hosts, or privileged ports.
package netutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// CA is a local certificate authority. EnsureCA populates the fields and
// loads the parsed certificate/key for signing leaf certs.
type CA struct {
	CertPEM  string // PEM-encoded CA certificate
	KeyPath  string // path to the CA private key file
	CertPath string // path to the CA certificate file

	cert *x509.Certificate // parsed CA certificate
	key  *ecdsa.PrivateKey // parsed CA private key
}

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"

	// caOrg is the organisation name embedded in CA and leaf subjects.
	caOrg = "stunt"

	// CA validity: 10 years (mkcert-style long-lived local root).
	caValidity = 10 * 365 * 24 * time.Hour

	// Leaf validity: 825 days (matches macOS trust policy maximum).
	leafValidity = 825 * 24 * time.Hour
)

// EnsureCA loads the CA from dir if it already exists, or generates a new
// self-signed root CA (ECDSA P-256) and writes <dir>/ca.pem and
// <dir>/ca-key.pem. The generated root is mkcert-style: a single local CA
// trusted manually by the developer via `stunt trust`.
//
// If only one of the cert/key files exists (partial state), EnsureCA returns
// an error rather than silently regenerating — a regenerated cert would
// orphan the old, possibly-trusted certificate. The user should run
// `stunt clean` to remove the partial state first (I6).
//
// ECDSA P-256 is chosen for small, modern certs with fast signing/verification.
func EnsureCA(dir string) (*CA, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)

	// If both files exist, load them.
	if certExists && keyExists {
		return loadCA(certPath, keyPath)
	}

	// Partial state: one file exists without the other. Do NOT silently
	// regenerate — that would orphan a possibly-trusted cert (I6).
	if certExists || keyExists {
		return nil, fmt.Errorf("netutil: partial CA state in %s (cert exists: %v, key exists: %v); run 'stunt clean' to remove the orphaned files and regenerate", dir, certExists, keyExists)
	}

	return generateCA(certPath, keyPath)
}

// Leaf mints a leaf TLS certificate for host, signed by the CA. The leaf is
// valid for the given host plus the default local SANs: localhost,
// *.localhost, and the IP address 127.0.0.1. Returns PEM-encoded certificate
// and private key bytes.
func (c *CA) Leaf(host string) (certPEM, keyPEM []byte, err error) {
	// Generate the leaf private key (ECDSA P-256).
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("netutil: generate leaf key: %w", err)
	}

	// Build a de-duplicated SAN list.
	dnsNames := sanDNSNames(host)
	ips := []netIP{ip127()}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{caOrg},
			CommonName:   host,
		},
		NotBefore:             time.Now().Add(-time.Hour), // 1h clock skew tolerance
		NotAfter:              time.Now().Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:              dnsNames,
		IPAddresses:           ips,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &leafKey.PublicKey, c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("netutil: sign leaf cert: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return nil, nil, fmt.Errorf("netutil: marshal leaf key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM, nil
}

// --- CA generation / loading ---

func generateCA(certPath, keyPath string) (*CA, error) {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("netutil: generate CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{caOrg},
			CommonName:   caOrg + " Local CA",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(caValidity),
		KeyUsage:  x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0, // this CA can only sign leaf certs, not sub-CAs
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, fmt.Errorf("netutil: create CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalPKCS8PrivateKey(rootKey)
	if err != nil {
		return nil, fmt.Errorf("netutil: marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// Ensure directory exists.
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return nil, fmt.Errorf("netutil: create CA dir: %w", err)
	}

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("netutil: write CA cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("netutil: write CA key: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("netutil: parse CA cert: %w", err)
	}

	return &CA{
		CertPEM:  string(certPEM),
		KeyPath:  keyPath,
		CertPath: certPath,
		cert:     cert,
		key:      rootKey,
	}, nil
}

func loadCA(certPath, keyPath string) (*CA, error) {
	certPEMBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("netutil: read CA cert: %w", err)
	}
	keyPEMBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("netutil: read CA key: %w", err)
	}

	block, _ := pem.Decode(certPEMBytes)
	if block == nil {
		return nil, errors.New("netutil: CA cert is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("netutil: parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEMBytes)
	if keyBlock == nil {
		return nil, errors.New("netutil: CA key is not valid PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("netutil: parse CA key: %w", err)
	}
	key, ok := keyAny.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("netutil: CA key is %T, want *ecdsa.PrivateKey", keyAny)
	}

	return &CA{
		CertPEM:  string(certPEMBytes),
		KeyPath:  keyPath,
		CertPath: certPath,
		cert:     cert,
		key:      key,
	}, nil
}

// --- Platform detection ---

// Platform returns a normalised platform identifier for trust-store logic:
//
//   - "darwin"        — macOS
//   - "windows"       — Windows
//   - "linux-debian"  — Debian/Ubuntu and derivatives (update-ca-certificates)
//   - "linux-rhel"    — RHEL/CentOS/Fedora (update-ca-trust)
//   - "linux-other"   — other Linux distributions
func Platform() string {
	return detectPlatform(runtime.GOOS)
}

func detectPlatform(goos string) string {
	switch goos {
	case "darwin":
		return "darwin"
	case "windows":
		return "windows"
	case "linux":
		return detectLinuxFlavor()
	default:
		return goos
	}
}

func detectLinuxFlavor() string {
	// Debian/Ubuntu derivatives have /etc/debian_version.
	if fileExists("/etc/debian_version") {
		return "linux-debian"
	}
	// RHEL/CentOS/Fedora have /etc/redhat-release.
	if fileExists("/etc/redhat-release") {
		return "linux-rhel"
	}
	return "linux-other"
}

// --- Trust commands (construct only; never executed here) ---

// TrustCommand constructs the platform-appropriate command to install caPath
// into the system trust store. The returned *exec.Cmd is NOT started; the
// caller (typically `stunt trust`) is responsible for running it.
func TrustCommand(caPath string) (*exec.Cmd, error) {
	return trustCommandFor(Platform(), caPath)
}

func trustCommandFor(platform, caPath string) (*exec.Cmd, error) {
	switch platform {
	case "darwin":
		return exec.Command("security", "add-trusted-cert",
			"-d",
			"-r", "trustRoot",
			"-k", "/Library/Keychains/System.keychain",
			caPath,
		), nil
	case "linux-debian":
		target := "/usr/local/share/ca-certificates/stunt.crt"
		script := fmt.Sprintf("cp %s %s && update-ca-certificates", shellQuote(caPath), shellQuote(target))
		return exec.Command("sh", "-c", script), nil
	case "linux-rhel":
		target := "/etc/pki/ca-trust/source/anchors/stunt.crt"
		script := fmt.Sprintf("cp %s %s && update-ca-trust extract", shellQuote(caPath), shellQuote(target))
		return exec.Command("sh", "-c", script), nil
	case "windows":
		return exec.Command("certutil", "-addstore", "-f", "Root", caPath), nil
	case "linux-other":
		return nil, fmt.Errorf("netutil: automatic trust not supported on this Linux distribution; install %s manually", caPath)
	default:
		return nil, fmt.Errorf("netutil: unsupported platform %q", platform)
	}
}

// UntrustCommand constructs the platform-appropriate command to remove the CA
// from the system trust store. The returned *exec.Cmd is NOT started.
func UntrustCommand(caPath string) (*exec.Cmd, error) {
	return untrustCommandFor(Platform(), caPath)
}

func untrustCommandFor(platform, caPath string) (*exec.Cmd, error) {
	switch platform {
	case "darwin":
		// Delete by the CA common name. The `-c` flag matches on the cert name.
		return exec.Command("security", "delete-certificate",
			"-c", caOrg+" Local CA",
		), nil
	case "linux-debian":
		target := "/usr/local/share/ca-certificates/stunt.crt"
		script := fmt.Sprintf("rm -f %s && update-ca-certificates --fresh", shellQuote(target))
		return exec.Command("sh", "-c", script), nil
	case "linux-rhel":
		target := "/etc/pki/ca-trust/source/anchors/stunt.crt"
		script := fmt.Sprintf("rm -f %s && update-ca-trust extract", shellQuote(target))
		return exec.Command("sh", "-c", script), nil
	case "windows":
		return exec.Command("certutil", "-delstore", "Root", caOrg+" Local CA"), nil
	case "linux-other":
		return nil, fmt.Errorf("netutil: automatic untrust not supported on this Linux distribution; remove %s manually", caPath)
	default:
		return nil, fmt.Errorf("netutil: unsupported platform %q", platform)
	}
}

// --- internal helpers ---

// shellQuote wraps a string in single quotes for safe use in a POSIX shell
// command, escaping any embedded single quotes. This prevents command
// substitution ($(...)), backticks, and other shell metacharacters inside
// the string from being executed (I1).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// netIP is an alias for net.IP to keep the Leaf method readable.
type netIP = net.IP

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("netutil: generate serial: %w", err)
	}
	return serial, nil
}

// ip127 returns the loopback address 127.0.0.1.
func ip127() net.IP {
	return net.IPv4(127, 0, 0, 1)
}

// sanDNSNames builds a de-duplicated, ordered list of DNS SANs for a leaf
// cert. The host itself is included along with the defaults: localhost and
// *.localhost. Duplicates are removed.
func sanDNSNames(host string) []string {
	seen := make(map[string]bool)
	var result []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	add(host)
	add("localhost")
	add("*.localhost")
	return result
}
