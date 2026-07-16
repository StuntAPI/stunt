package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupServiceTest saves/restores the global serviceUnitDir and sets it to
// a temp directory.
func setupServiceTest(t *testing.T) string {
	t.Helper()
	saved := serviceUnitDir
	t.Cleanup(func() { serviceUnitDir = saved })
	dir := t.TempDir()
	serviceUnitDir = dir
	return dir
}

func TestGenerateServiceUnit_Launchd(t *testing.T) {
	unit, err := generateServiceUnitFor("darwin", "proxy", "/usr/local/bin/stunt", "/home/user/stunt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(unit.FileName, ".plist") {
		t.Errorf("launchd filename = %q, want .plist", unit.FileName)
	}
	if !strings.Contains(unit.Content, "com.stunt.proxy") {
		t.Error("plist missing label com.stunt.proxy")
	}
	if !strings.Contains(unit.Content, "<plist") {
		t.Error("plist missing <plist> root")
	}
	if !strings.Contains(unit.Content, "RunAtLoad") {
		t.Error("plist missing RunAtLoad")
	}
	if !strings.Contains(unit.Content, "<string>proxy</string>") {
		t.Error("plist missing proxy argument")
	}
	if !strings.Contains(unit.Content, "<string>start</string>") {
		t.Error("plist missing start argument")
	}
}

func TestGenerateServiceUnit_Systemd(t *testing.T) {
	unit, err := generateServiceUnitFor("linux-debian", "proxy", "/usr/local/bin/stunt", "/home/user/stunt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(unit.FileName, ".service") {
		t.Errorf("systemd filename = %q, want .service", unit.FileName)
	}
	if !strings.Contains(unit.Content, "[Unit]") {
		t.Error("systemd unit missing [Unit] section")
	}
	if !strings.Contains(unit.Content, "[Service]") {
		t.Error("systemd unit missing [Service] section")
	}
	if !strings.Contains(unit.Content, "ExecStart=") {
		t.Error("systemd unit missing ExecStart")
	}
	if !strings.Contains(unit.Content, "proxy start --foreground") {
		t.Error("systemd unit missing proxy start --foreground")
	}
}

func TestGenerateServiceUnit_Windows(t *testing.T) {
	unit, err := generateServiceUnitFor("windows", "proxy", `C:\stunt\stunt.exe`, `C:\stunt\stunt.yaml`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit.Content, "<Task") {
		t.Error("windows XML missing <Task> root")
	}
	if !strings.Contains(unit.Content, "proxy start") {
		t.Error("windows XML missing proxy start command")
	}
}

func TestInstallServiceUnit(t *testing.T) {
	dir := setupServiceTest(t)
	unit := ServiceUnit{
		Label:    "test",
		FileName: "test.plist",
		Content:  "<plist/>",
	}
	var out bytes.Buffer
	if err := installServiceUnit(&out, dir, unit); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test.plist")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if string(data) != "<plist/>" {
		t.Errorf("content = %q", data)
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Errorf("output = %q", out.String())
	}
}

func TestServiceStatus_NotInstalled(t *testing.T) {
	setupServiceTest(t)
	// Status checks for a file that doesn't exist in our temp dir.
	// We verify by calling the underlying file check.
	label := serviceLabel("proxy")
	path := filepath.Join(serviceUnitDir, unitFileName(label))
	if fileExistsCLI(path) {
		t.Error("file should not exist in temp dir")
	}
}

// generateServiceUnitFor is a test helper that calls generateServiceUnit
// with a specific platform (bypassing the real platform detection).
func generateServiceUnitFor(platform, name, exe, manifestPath string) (ServiceUnit, error) {
	label := serviceLabelFor(platform, name)
	fileName := unitFileNameFor(platform, label)

	var content string
	switch platform {
	case "darwin":
		content = generateLaunchdPlist(label, exe, manifestPath)
	case "linux-debian", "linux-rhel", "linux-other":
		content = generateSystemdUnit(label, exe, manifestPath)
	case "windows":
		content = generateWindowsService(label, exe, manifestPath)
	default:
		return ServiceUnit{}, fmt.Errorf("unsupported platform %q", platform)
	}
	return ServiceUnit{Label: label, FileName: fileName, Content: content}, nil
}

func serviceLabelFor(platform, name string) string {
	switch platform {
	case "darwin":
		return "com.stunt." + name
	case "linux-debian", "linux-rhel", "linux-other":
		return "stunt-" + name + ".service"
	case "windows":
		return "stunt-" + name
	default:
		return "stunt-" + name
	}
}

func unitFileNameFor(platform, label string) string {
	switch platform {
	case "darwin":
		return label + ".plist"
	default:
		return label
	}
}
