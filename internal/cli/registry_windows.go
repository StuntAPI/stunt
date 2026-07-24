//go:build windows

package cli

// withFlock is a no-op on Windows: the registry's atomic temp-rename still
// guards against torn reads, and lost updates from a concurrent stunt up/down
// race are healed by PID-pruning on the next List. (File locking on Windows
// would need LockFileEx; the no-op is acceptable for a localhost dev tool.)
func withFlock(lockPath string, fn func() error) error {
	return fn()
}

// pidAlive optimistically returns true on Windows: signal-0 liveness probing
// isn't supported by the syscall package, but pruning is a best-effort nicety,
// not a correctness gate (the entry also stores the manifest path, so a
// recycled PID is detectable on inspection).
func pidAlive(pid int) bool {
	return pid > 0
}
