//go:build !windows

package cli

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// withFlock holds an exclusive flock on lockPath for the duration of fn. Unix only.
func withFlock(lockPath string, fn func() error) error {
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer lf.Close()
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("flock %s: %w", lockPath, err)
	}
	defer unix.Flock(int(lf.Fd()), unix.LOCK_UN)
	return fn()
}

// pidAlive reports whether pid is currently running (signal-0 liveness check,
// mirroring stunt down). Unix only.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
