package netutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSeedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return string(data)
}

func TestSyncHostsCreatesBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n255.255.255.255 broadcasthost\n")

	err := SyncHosts(path, []HostEntry{
		{Host: "api.localhost"},
		{Host: "web.localhost"},
	})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}

	content := readFile(t, path)

	// Original content preserved.
	if !strings.Contains(content, "127.0.0.1 localhost\n") {
		t.Error("original localhost entry lost")
	}
	if !strings.Contains(content, "255.255.255.255 broadcasthost\n") {
		t.Error("original broadcasthost entry lost")
	}

	// Managed block present.
	if !strings.Contains(content, beginMarker) {
		t.Error("missing BEGIN stunt marker")
	}
	if !strings.Contains(content, endMarker) {
		t.Error("missing END stunt marker")
	}
	if !strings.Contains(content, "127.0.0.1 api.localhost") {
		t.Error("missing api.localhost entry")
	}
	if !strings.Contains(content, "127.0.0.1 web.localhost") {
		t.Error("missing web.localhost entry")
	}
}

func TestSyncHostsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	entries := []HostEntry{{Host: "api.localhost"}, {Host: "web.localhost"}}

	if err := SyncHosts(path, entries); err != nil {
		t.Fatalf("first SyncHosts: %v", err)
	}
	first := readFile(t, path)

	if err := SyncHosts(path, entries); err != nil {
		t.Fatalf("second SyncHosts: %v", err)
	}
	second := readFile(t, path)

	if first != second {
		t.Errorf("SyncHosts is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestSyncHostsReplacesBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	// First sync with two entries.
	if err := SyncHosts(path, []HostEntry{
		{Host: "old.localhost"},
	}); err != nil {
		t.Fatalf("first SyncHosts: %v", err)
	}

	// Second sync with a different entry.
	if err := SyncHosts(path, []HostEntry{
		{Host: "new.localhost"},
	}); err != nil {
		t.Fatalf("second SyncHosts: %v", err)
	}

	content := readFile(t, path)

	if strings.Contains(content, "old.localhost") {
		t.Error("old entry should have been replaced")
	}
	if !strings.Contains(content, "127.0.0.1 new.localhost") {
		t.Error("missing new entry")
	}

	// Only one managed block.
	if c := strings.Count(content, beginMarker); c != 1 {
		t.Errorf("expected 1 BEGIN marker, got %d", c)
	}
}

func TestSyncHostsPreservesSurroundingContent(t *testing.T) {
	seed := strings.Join([]string{
		"# My custom comment",
		"127.0.0.1 localhost",
		"255.255.255.255 broadcasthost",
		"::1 localhost",
		"",
		"# Another comment",
		"192.168.1.1 myrouter",
		"",
	}, "\n")

	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, seed)

	if err := SyncHosts(path, []HostEntry{{Host: "svc.localhost"}}); err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}

	content := readFile(t, path)

	for _, want := range []string{
		"# My custom comment",
		"255.255.255.255 broadcasthost",
		"::1 localhost",
		"# Another comment",
		"192.168.1.1 myrouter",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("surrounding content lost: %q not found in:\n%s", want, content)
		}
	}
}

func TestSyncHostsEmptyEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	err := SyncHosts(path, nil)
	if err != nil {
		t.Fatalf("SyncHosts with nil entries: %v", err)
	}

	content := readFile(t, path)
	if strings.Contains(content, beginMarker) {
		t.Error("empty entries should not create a managed block")
	}
}

func TestSyncHostsNoExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")

	err := SyncHosts(path, []HostEntry{{Host: "api.localhost"}})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}

	content := readFile(t, path)
	if !strings.Contains(content, "127.0.0.1 api.localhost") {
		t.Errorf("entry missing in:\n%s", content)
	}
}

func TestCleanHostsRemovesBlock(t *testing.T) {
	seed := "127.0.0.1 localhost\n"

	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, seed)

	if err := SyncHosts(path, []HostEntry{
		{Host: "api.localhost"},
		{Host: "web.localhost"},
	}); err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}

	if err := CleanHosts(path); err != nil {
		t.Fatalf("CleanHosts: %v", err)
	}

	content := readFile(t, path)

	if strings.Contains(content, beginMarker) {
		t.Error("BEGIN marker still present after CleanHosts")
	}
	if strings.Contains(content, endMarker) {
		t.Error("END marker still present after CleanHosts")
	}
	if strings.Contains(content, "api.localhost") {
		t.Error("stunt entry still present after CleanHosts")
	}
	if !strings.Contains(content, "127.0.0.1 localhost") {
		t.Error("original content lost after CleanHosts")
	}
}

func TestCleanHostsNoBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n192.168.1.1 router\n")

	err := CleanHosts(path)
	if err != nil {
		t.Fatalf("CleanHosts: %v", err)
	}

	content := readFile(t, path)
	// Should be unchanged.
	if content != "127.0.0.1 localhost\n192.168.1.1 router\n" {
		t.Errorf("content changed when no block existed:\n%s", content)
	}
}

func TestCleanHostsNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")

	err := CleanHosts(path)
	if err != nil {
		t.Fatalf("CleanHosts on non-existent file: %v", err)
	}
}

func TestSpoofHostsCreatesBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	err := SpoofHosts(path, map[string]string{
		"api.stripe.com": "127.0.0.1",
		"api.github.com": "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("SpoofHosts: %v", err)
	}

	content := readFile(t, path)

	if !strings.Contains(content, "127.0.0.1 api.stripe.com") {
		t.Error("missing api.stripe.com spoof entry")
	}
	if !strings.Contains(content, "127.0.0.1 api.github.com") {
		t.Error("missing api.github.com spoof entry")
	}
	if !strings.Contains(content, beginMarker) {
		t.Error("missing managed block markers")
	}

	// Original content preserved.
	if !strings.Contains(content, "127.0.0.1 localhost\n") {
		t.Error("original localhost entry lost")
	}
}

func TestSpoofHostsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	services := map[string]string{
		"api.stripe.com": "127.0.0.1",
	}

	if err := SpoofHosts(path, services); err != nil {
		t.Fatalf("first SpoofHosts: %v", err)
	}
	first := readFile(t, path)

	if err := SpoofHosts(path, services); err != nil {
		t.Fatalf("second SpoofHosts: %v", err)
	}
	second := readFile(t, path)

	if first != second {
		t.Errorf("SpoofHosts is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestSpoofHostsDeterministicOrder(t *testing.T) {
	path1 := filepath.Join(t.TempDir(), "hosts1")
	path2 := filepath.Join(t.TempDir(), "hosts2")

	services := map[string]string{
		"api.stripe.com":  "127.0.0.1",
		"api.github.com":  "127.0.0.1",
		"api.example.com": "127.0.0.1",
	}

	if err := SpoofHosts(path1, services); err != nil {
		t.Fatalf("SpoofHosts path1: %v", err)
	}
	if err := SpoofHosts(path2, services); err != nil {
		t.Fatalf("SpoofHosts path2: %v", err)
	}

	c1 := readFile(t, path1)
	c2 := readFile(t, path2)

	if c1 != c2 {
		t.Errorf("output is not deterministic across runs:\n--- run1 ---\n%s\n--- run2 ---\n%s", c1, c2)
	}
}

func TestCleanAfterSpoof(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	if err := SpoofHosts(path, map[string]string{
		"api.stripe.com": "127.0.0.1",
	}); err != nil {
		t.Fatalf("SpoofHosts: %v", err)
	}

	if err := CleanHosts(path); err != nil {
		t.Fatalf("CleanHosts: %v", err)
	}

	content := readFile(t, path)
	if strings.Contains(content, "api.stripe.com") {
		t.Error("spoof entry survived CleanHosts")
	}
}

func TestDefaultHostsFile(t *testing.T) {
	if DefaultHostsFile != "/etc/hosts" {
		t.Errorf("DefaultHostsFile = %q, want %q", DefaultHostsFile, "/etc/hosts")
	}
}

func TestSyncHostsDeduplicatesEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")

	err := SyncHosts(path, []HostEntry{
		{Host: "dup.localhost"},
		{Host: "dup.localhost"},
		{Host: "other.localhost"},
	})
	if err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}

	content := readFile(t, path)
	count := strings.Count(content, "127.0.0.1 dup.localhost")
	if count != 1 {
		t.Errorf("expected 1 occurrence of dup.localhost, got %d", count)
	}
}

func TestBlockLocationPreservesLeadingContent(t *testing.T) {
	// If a managed block already exists, replacing it should preserve
	// all content before and after it.
	seed := strings.Join([]string{
		"# header",
		"127.0.0.1 localhost",
		beginMarker,
		"127.0.0.1 old.localhost",
		endMarker,
		"# footer",
		"",
	}, "\n")

	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, seed)

	if err := SyncHosts(path, []HostEntry{{Host: "new.localhost"}}); err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}

	content := readFile(t, path)
	lines := strings.Split(content, "\n")

	// header before block.
	foundHeader := false
	foundFooter := false
	foundNew := false
	for _, l := range lines {
		if l == "# header" {
			foundHeader = true
		}
		if l == "# footer" {
			foundFooter = true
		}
		if l == "127.0.0.1 new.localhost" {
			foundNew = true
		}
	}
	if !foundHeader {
		t.Error("header content lost")
	}
	if !foundFooter {
		t.Error("footer content lost")
	}
	if !foundNew {
		t.Error("new entry missing")
	}
	if strings.Contains(content, "old.localhost") {
		t.Error("old entry should have been replaced")
	}
}

func TestSyncHostsSortedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")

	entries := []HostEntry{
		{Host: "zebra.localhost"},
		{Host: "alpha.localhost"},
		{Host: "mango.localhost"},
	}
	if err := SyncHosts(path, entries); err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}

	content := readFile(t, path)

	// Entries should appear in sorted order.
	alphaIdx := strings.Index(content, "alpha.localhost")
	mangoIdx := strings.Index(content, "mango.localhost")
	zebraIdx := strings.Index(content, "zebra.localhost")

	if !(alphaIdx < mangoIdx && mangoIdx < zebraIdx) {
		t.Errorf("entries not sorted in output:\n%s", content)
	}
}

// --- C1: injection validation tests ---

func TestSyncHostsRejectsNewlineInjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	// A hostname that tries to inject content outside the managed block.
	evil := "evil\n# END stunt\n0.0.0.0 attacker"
	err := SyncHosts(path, []HostEntry{{Host: evil}})
	if err == nil {
		t.Fatal("SyncHosts should reject hostname with newlines")
	}

	// The file should be unchanged (nothing written).
	content := readFile(t, path)
	if strings.Contains(content, "attacker") {
		t.Error("injected content leaked into hosts file")
	}
	if strings.Contains(content, beginMarker) {
		t.Error("managed block should not have been written")
	}
}

func TestSyncHostsRejectsSpaceInHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	err := SyncHosts(path, []HostEntry{{Host: "evil host"}})
	if err == nil {
		t.Fatal("SyncHosts should reject hostname with spaces")
	}
}

func TestSyncHostsRejectsTabInHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	err := SyncHosts(path, []HostEntry{{Host: "evil\thost"}})
	if err == nil {
		t.Fatal("SyncHosts should reject hostname with tabs")
	}
}

func TestSyncHostsValidMultiSegmentHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")

	err := SyncHosts(path, []HostEntry{{Host: "api.v2.stripe.localhost"}})
	if err != nil {
		t.Fatalf("SyncHosts should accept multi-segment host: %v", err)
	}
	content := readFile(t, path)
	if !strings.Contains(content, "127.0.0.1 api.v2.stripe.localhost") {
		t.Errorf("missing valid multi-segment entry:\n%s", content)
	}
}

func TestSpoofHostsRejectsNewlineInHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	err := SpoofHosts(path, map[string]string{
		"evil\n# END stunt\n0.0.0.0 attacker": "127.0.0.1",
	})
	if err == nil {
		t.Fatal("SpoofHosts should reject hostname with newlines")
	}
	content := readFile(t, path)
	if strings.Contains(content, "attacker") {
		t.Error("injected content leaked into hosts file")
	}
}

func TestSpoofHostsRejectsNewlineInIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	err := SpoofHosts(path, map[string]string{
		"api.example.com": "127.0.0.1\n# END stunt\n0.0.0.0 attacker",
	})
	if err == nil {
		t.Fatal("SpoofHosts should reject IP with newlines")
	}
	content := readFile(t, path)
	if strings.Contains(content, "attacker") {
		t.Error("injected content leaked into hosts file")
	}
}

// --- I2: atomic write tests ---

func TestAtomicWritePreservesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")

	// Write, then read back and verify exact content.
	entries := []HostEntry{{Host: "svc1.localhost"}, {Host: "svc2.localhost"}}
	if err := SyncHosts(path, entries); err != nil {
		t.Fatalf("SyncHosts: %v", err)
	}
	content := readFile(t, path)

	// The content should contain both entries and both markers exactly once.
	if strings.Count(content, beginMarker) != 1 {
		t.Errorf("expected 1 BEGIN marker, got %d", strings.Count(content, beginMarker))
	}
	if strings.Count(content, endMarker) != 1 {
		t.Errorf("expected 1 END marker, got %d", strings.Count(content, endMarker))
	}

	// No leftover temp files in the directory.
	dir := filepath.Dir(path)
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, f := range files {
		name := f.Name()
		if strings.HasPrefix(name, ".stunt-hosts-") {
			t.Errorf("leftover temp file: %s", name)
		}
	}
}

func TestCleanHostsAtomicWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	writeSeedFile(t, path, "127.0.0.1 localhost\n")

	if err := SyncHosts(path, []HostEntry{{Host: "svc.localhost"}}); err != nil {
		t.Fatal(err)
	}
	if err := CleanHosts(path); err != nil {
		t.Fatal(err)
	}

	content := readFile(t, path)
	if strings.Contains(content, "svc.localhost") {
		t.Error("managed entry survived CleanHosts")
	}
	if !strings.Contains(content, "127.0.0.1 localhost") {
		t.Error("original content lost after CleanHosts")
	}

	// No leftover temp files.
	dir := filepath.Dir(path)
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".stunt-hosts-") {
			t.Errorf("leftover temp file: %s", f.Name())
		}
	}
}

func TestValidateHost(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"empty", "", true},
		{"space", "foo bar", true},
		{"tab", "foo\tbar", true},
		{"newline", "foo\nbar", true},
		{"carriage-return", "foo\rbar", true},
		{"valid-simple", "localhost", false},
		{"valid-dotted", "api.stripe.com", false},
		{"valid-wildcard", "*.localhost", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateHost(c.host)
			if c.wantErr && err == nil {
				t.Errorf("validateHost(%q) should have returned error", c.host)
			}
			if !c.wantErr && err != nil {
				t.Errorf("validateHost(%q) returned unexpected error: %v", c.host, err)
			}
		})
	}
}

func TestValidateIP(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		wantErr bool
	}{
		{"empty", "", true},
		{"space", "127.0.0.1 evil", true},
		{"newline", "127.0.0.1\nevil", true},
		{"valid-ipv4", "127.0.0.1", false},
		{"valid-ipv6", "::1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateIP(c.ip)
			if c.wantErr && err == nil {
				t.Errorf("validateIP(%q) should have returned error", c.ip)
			}
			if !c.wantErr && err != nil {
				t.Errorf("validateIP(%q) returned unexpected error: %v", c.ip, err)
			}
		})
	}
}
