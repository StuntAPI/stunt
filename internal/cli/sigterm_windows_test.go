//go:build windows

package cli

// selfSIGTERM is a no-op on Windows (no SIGTERM). Tests that rely on it to stop
// a backgrounded stunt up should be skipped on Windows; this avoids the
// syscall.Kill symbol being compiled there.
func selfSIGTERM() {}
