//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// procAlive reports whether the process with the given PID is currently
// running. This is the platform liveness primitive shared by `stunt stop`,
// `stunt down`, and the global instance registry. On Unix it uses the
// signal-0 (kill -0) probe: a nil error means the process exists.
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// gracefulStop asks the process to shut down politely. On Unix this is
// SIGTERM, which the stunt server's signal handler turns into a clean
// http.Server.Shutdown. The caller follows up with waitForExit and
// escalates to proc.Kill() if the process does not exit in time.
func gracefulStop(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}
