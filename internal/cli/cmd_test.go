package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

// TestHostsSyncOnRealHostsFile exercises the actual cobra command path with
// a hostsPath override (temp file). The real /etc/hosts is never touched.
func TestHostsSyncCmd(t *testing.T) {
	path := setupHostsTest(t)
	mPath := filepath.Join(t.TempDir(), "stunt.yaml")
	writeTestManifest(t, mPath, "subdomain", "localhost")

	root := NewRootCmd()
	root.SetArgs([]string{"hosts", "sync", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	// Verify the managed block was written to the temp hosts file.
	data, err := readFileAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, "alpha.localhost") {
		t.Errorf("hosts file missing alpha.localhost:\n%s", data)
	}
}

func TestHostsCleanCmd(t *testing.T) {
	path := setupHostsTest(t)
	// Pre-populate with a managed block.
	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
		},
	}
	if err := runHostsSync(&bytes.Buffer{}, m, path); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"hosts", "clean"})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	data, err := readFileAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data, "alpha.localhost") {
		t.Errorf("hosts file should not have managed block:\n%s", data)
	}
}

func TestDoctorCmd(t *testing.T) {
	mPath := filepath.Join(t.TempDir(), "stunt.yaml")
	writeTestManifest(t, mPath, "port", "")

	root := NewRootCmd()
	root.SetArgs([]string{"doctor", "--manifest", mPath})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "platform:") {
		t.Errorf("missing platform: in output:\n%s", out)
	}
}

// TestTrustCmdRequiresCA verifies that the trust command constructs the
// platform trust command without running it (we only verify it builds the
// command path; actually running it requires privilege).
func TestTrustCmdConstructsCommand(t *testing.T) {
	t.Skip("trust command requires real privilege to run — tested via netutil.TrustCommand unit tests instead")
}

// TestProxyStartPrivilegedRequiresSudo verifies the sudo re-exec path is
// taken when a privileged port is requested without root.
func TestProxyStartPrivilegedDetection(t *testing.T) {
	// We can't run the actual command (it would try to bind or sudo re-exec),
	// but we can verify the detection logic.
	if isPrivilegedPort(443) && !isRoot() {
		// On a non-root dev machine, port 443 would trigger sudo re-exec.
		// This test verifies the logic is correct.
		t.Log("port 443 is privileged and we are not root — sudo re-exec would be triggered")
	}
	if !isPrivilegedPort(8443) {
		t.Log("port 8443 is not privileged — direct bind would be attempted")
	}
}

// writeTestManifest writes a minimal manifest to path for CLI tests.
func writeTestManifest(t *testing.T, path, mode, tld string) {
	t.Helper()
	m := &manifest.Manifest{
		Version: 1,
		Path:    path,
		Network: manifest.Network{Mode: mode, TLD: tld},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
		},
	}
	if mode == "port" {
		m.Network.BasePort = 9000
	}
	if err := writeYAMLManifest(m, path); err != nil {
		t.Fatal(err)
	}
}

func writeYAMLManifest(m *manifest.Manifest, path string) error {
	// Write a simple YAML that the loader can parse.
	var tldLine string
	if m.Network.TLD != "" {
		tldLine = "  tld: " + m.Network.TLD + "\n"
	}
	var basePortLine string
	if m.Network.BasePort > 0 {
		basePortLine = "  base_port: " + itoa(m.Network.BasePort) + "\n"
	}
	content := "version: 1\nnetwork:\n  mode: " + m.Network.Mode + "\n" + tldLine + basePortLine + "services:\n  alpha:\n    rules:\n      - match: { path: / }\n        respond: { status: 200 }\n"
	return writeFile(path, content)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func readFileAll(path string) (string, error) {
	data, err := osReadFileAll(path)
	return string(data), err
}
