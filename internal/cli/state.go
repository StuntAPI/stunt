package cli

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// stuntSubdir is the directory (relative to the manifest dir) where stunt
// stores its local state: CA, engine databases, etc.
const stuntSubdir = ".stunt"

// caPath returns the directory for the stunt local CA, derived from the
// manifest directory.
func caPath(manifestDir string) string {
	return filepath.Join(manifestDir, stuntSubdir, "ca")
}

// statePath returns the directory for per-service state databases.
func statePath(manifestDir string) string {
	return filepath.Join(manifestDir, stuntSubdir, "state")
}

// manifestDir returns the directory containing the manifest file.
func manifestDir(manifestPath string) string {
	return filepath.Dir(manifestPath)
}

// isPrivilegedPort returns true if the given TCP port number requires root
// to bind on Unix systems (ports < 1024).
func isPrivilegedPort(port int) bool {
	return port > 0 && port < 1024
}

// isRoot returns true if the current process is running as root (uid 0).
func isRoot() bool {
	return os.Geteuid() == 0
}

// sudoReexecCmd constructs the command to re-exec the current binary under
// sudo. It does NOT start the command; the caller decides whether to run it.
func sudoReexecCmd(extraArgs ...string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := append([]string{exe}, extraArgs...)
	return exec.Command("sudo", args...), nil
}

// portFromAddr extracts the numeric port from an address string like
// ":443" or "127.0.0.1:8443". Returns 0 if the port cannot be parsed.
func portFromAddr(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

// osStat wraps os.Stat for use in other CLI files without repeating the import.
func osStat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

// osWriteFile wraps os.WriteFile for test helpers.
func osWriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// osReadFileAll wraps os.ReadFile for test helpers.
func osReadFileAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}
