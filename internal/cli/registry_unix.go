//go:build !windows

package cli

import (
	"fmt"
	"os"

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

// procAlive (the Unix signal-0 liveness primitive) and gracefulStop live in
// proc_unix.go. The registry shares that same primitive so liveness semantics
// are identical for `stunt stop`, `stunt down`, and registry pruning.
