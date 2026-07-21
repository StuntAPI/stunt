package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/rules"
)

// setupHostsTest creates a temp hosts file and saves/restores the global
// hostsPath so tests don't touch the real /etc/hosts.
func setupHostsTest(t *testing.T) string {
	t.Helper()
	saved := hostsPath
	t.Cleanup(func() { hostsPath = saved })
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostsPath = path
	return path
}

func TestRunHostsSync(t *testing.T) {
	path := setupHostsTest(t)
	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"alpha": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
			"beta":  {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
		},
	}
	var out bytes.Buffer
	if err := runHostsSync(&out, m, path); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	s := string(content)
	if !strings.Contains(s, "alpha.localhost") {
		t.Errorf("missing alpha.localhost:\n%s", s)
	}
	if !strings.Contains(s, "beta.localhost") {
		t.Errorf("missing beta.localhost:\n%s", s)
	}
	if !strings.Contains(out.String(), "synced 2 host") {
		t.Errorf("output = %q", out.String())
	}
}

func TestRunHostsClean(t *testing.T) {
	path := setupHostsTest(t)
	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "subdomain", TLD: "localhost"},
		Services: map[string]manifest.Service{
			"svc": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
		},
	}
	// Sync first to create the block.
	if err := runHostsSync(&bytes.Buffer{}, m, path); err != nil {
		t.Fatal(err)
	}
	// Now clean.
	var out bytes.Buffer
	if err := runHostsClean(&out, path); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	s := string(content)
	if strings.Contains(s, "svc.localhost") {
		t.Errorf("svc.localhost still present after clean:\n%s", s)
	}
	if !strings.Contains(s, "127.0.0.1 localhost") {
		t.Errorf("original content lost:\n%s", s)
	}
}

func TestRunHostsSyncCustomTLD(t *testing.T) {
	path := setupHostsTest(t)
	m := &manifest.Manifest{
		Version: 1,
		Network: manifest.Network{Mode: "subdomain", TLD: "test"},
		Services: map[string]manifest.Service{
			"api": {Rules: []rules.Rule{{Match: rules.Match{Path: "/"}, Respond: rules.Respond{Status: 200}}}},
		},
	}
	if err := runHostsSync(&bytes.Buffer{}, m, path); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "api.test") {
		t.Errorf("missing api.test:\n%s", content)
	}
}
