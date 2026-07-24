//go:build !windows

package cli

import "syscall"

// selfSIGTERM sends SIGTERM to the current process (used by tests that boot a
// backgrounded `stunt up` and need to stop it). Unix only.
func selfSIGTERM() { _ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM) }
