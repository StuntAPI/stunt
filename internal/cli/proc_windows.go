//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not yet exited. x/sys/windows does not export this constant (it is
// #define STILL_ACTIVE 259 in winnt.h).
const stillActive = 259

// procAlive reports whether the process with the given PID is currently
// running. The Unix signal-0 liveness probe is unavailable on Windows —
// os.Process.Signal returns EWINDOWS ("not supported by windows") for any
// signal other than os.Kill — so we ask the kernel directly:
// OpenProcess + GetExitCodeProcess, treating STILL_ACTIVE as alive.
//
// PROCESS_QUERY_LIMITED_INFORMATION is sufficient for a liveness check and is
// available on processes the current user started (the dev-tool case), even
// when the stricter PROCESS_QUERY_INFORMATION right is not.
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false // gone, or not ours to open — treat as not alive
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// gracefulStop on Windows has no cross-process equivalent of SIGTERM: a Go
// process can only be terminated (TerminateProcess), not asked politely to
// shut down by PID. We therefore perform a hard stop via proc.Kill, which is
// acceptable for a localhost dev tool (in-flight requests are dropped; the
// port is freed). The caller still follows up with waitForExit; the
// post-timeout force escalation in stopPID/runDown is effectively a no-op
// here because the process is already gone.
func gracefulStop(proc *os.Process) error {
	return proc.Kill()
}
