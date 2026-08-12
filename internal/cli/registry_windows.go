//go:build windows

package cli

// withFlock is a no-op on Windows: the registry's atomic temp-rename still
// guards against torn reads, and lost updates from a concurrent stunt up/down
// race are healed by PID-pruning on the next List. (File locking on Windows
// would need LockFileEx; the no-op is acceptable for a localhost dev tool.)
func withFlock(lockPath string, fn func() error) error {
	return fn()
}

// procAlive (the Windows liveness primitive via OpenProcess +
// GetExitCodeProcess) and gracefulStop live in proc_windows.go. Unlike the
// earlier optimistic always-true stub, procAlive now reports real liveness,
// so registry pruning of dead entries actually works on Windows.
