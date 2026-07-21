package cli

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/netutil"
)

// serviceUnitDir is the directory where service unit files are installed at
// runtime. Tests override it with a temp directory.
var serviceUnitDir = defaultServiceUnitDir()

func defaultServiceUnitDir() string {
	switch netutil.Platform() {
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	case "linux-debian", "linux-rhel", "linux-other":
		return "/etc/systemd/system"
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "stunt")
	default:
		return filepath.Join(os.Getenv("HOME"), ".stunt", "service")
	}
}

// serviceLabel builds a reverse-DNS label for the unit (e.g.
// "com.stunt.proxy" on macOS, "stunt-proxy.service" on Linux).
func serviceLabel(name string) string {
	switch netutil.Platform() {
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

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install, inspect, or remove the stunt system service",
	}
	cmd.AddCommand(newServiceInstallCmd(), newServiceStatusCmd(), newServiceUninstallCmd())
	return cmd
}

func newServiceInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Write the service unit file (does not start the service)",
		RunE: func(cmd *cobra.Command, args []string) error {
			manifestPath, _ := cmd.Flags().GetString("manifest")
			exe, _ := os.Executable()
			unit, err := generateServiceUnit("proxy", exe, manifestPath)
			if err != nil {
				return err
			}
			return installServiceUnit(cmd.OutOrStdout(), serviceUnitDir, unit)
		},
	}
}

func newServiceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the service unit file is installed",
		RunE: func(cmd *cobra.Command, args []string) error {
			label := serviceLabel("proxy")
			path := filepath.Join(serviceUnitDir, unitFileName(label))
			if fileExistsCLI(path) {
				fmt.Fprintf(cmd.OutOrStdout(), "installed: %s\n", path)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "not installed: %s\n", path)
			}
			return nil
		},
	}
}

func newServiceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the service unit file (does not stop a running service)",
		RunE: func(cmd *cobra.Command, args []string) error {
			label := serviceLabel("proxy")
			path := filepath.Join(serviceUnitDir, unitFileName(label))
			if err := os.Remove(path); err != nil {
				if os.IsNotExist(err) {
					fmt.Fprintf(cmd.OutOrStdout(), "not installed: %s\n", path)
					return nil
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed: %s\n", path)
			return nil
		},
	}
}

// ServiceUnit is a generated service unit file.
type ServiceUnit struct {
	Label    string // platform label (e.g. "com.stunt.proxy")
	FileName string // file name within the unit dir
	Content  string // full file content
}

// unitFileName returns the file name for a given label.
func unitFileName(label string) string {
	switch netutil.Platform() {
	case "darwin":
		return label + ".plist"
	default:
		return label
	}
}

// generateServiceUnit builds the platform-specific service file for a stunt
// daemon. exe is the binary path, manifestPath is the --manifest argument.
func generateServiceUnit(name, exe, manifestPath string) (ServiceUnit, error) {
	label := serviceLabel(name)
	fileName := unitFileName(label)
	platform := netutil.Platform()

	var content string
	switch platform {
	case "darwin":
		content = generateLaunchdPlist(label, exe, manifestPath)
	case "linux-debian", "linux-rhel", "linux-other":
		content = generateSystemdUnit(label, exe, manifestPath)
	case "windows":
		content = generateWindowsService(label, exe, manifestPath)
	default:
		// Should not happen but handle gracefully.
		_ = runtime.GOOS
		return ServiceUnit{}, fmt.Errorf("unsupported platform %q for service install", platform)
	}

	return ServiceUnit{
		Label:    label,
		FileName: fileName,
		Content:  content,
	}, nil
}

// generateLaunchdPlist builds a macOS launchd plist for the stunt proxy.
// Paths are XML-escaped to prevent injection (I4).
func generateLaunchdPlist(label, exe, manifestPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>proxy</string>
        <string>start</string>
        <string>--foreground</string>
        <string>--manifest</string>
        <string>%s</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
`, html.EscapeString(label), html.EscapeString(exe), html.EscapeString(manifestPath))
}

// generateSystemdUnit builds a systemd unit file for the stunt proxy.
// Paths are quoted for systemd ExecStart parsing (I3).
func generateSystemdUnit(label, exe, manifestPath string) string {
	return fmt.Sprintf(`[Unit]
Description=stunt TLS proxy
After=network.target

[Service]
ExecStart=%s proxy start --foreground --manifest %s
Restart=on-failure

[Install]
WantedBy=multi-user.target
`, systemdQuote(exe), systemdQuote(manifestPath))
}

// systemdQuote wraps a path in double quotes and escapes backslashes and
// double quotes, following systemd ExecStart quoting rules (I3).
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return "\"" + s + "\""
}

// generateWindowsService builds a Windows Task Scheduler XML for the stunt
// proxy. Paths are XML-escaped to prevent injection (I4).
func generateWindowsService(label, exe, manifestPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <BootTrigger />
  </Triggers>
  <Actions>
    <Exec>
      <Command>%s</Command>
      <Arguments>proxy start --foreground --manifest %s</Arguments>
    </Exec>
  </Actions>
  <Settings>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
</Task>
`, html.EscapeString(exe), html.EscapeString(manifestPath))
}

// installServiceUnit writes the unit file to the target directory.
func installServiceUnit(out interface{ Write([]byte) (int, error) }, dir string, unit ServiceUnit) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create service dir: %w", err)
	}
	path := filepath.Join(dir, unit.FileName)
	if err := os.WriteFile(path, []byte(unit.Content), 0o644); err != nil {
		return fmt.Errorf("write service unit: %w", err)
	}
	fmt.Fprintf(out, "wrote %s\n", path)
	return nil
}
