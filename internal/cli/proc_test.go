package cli

import (
	"io"
	"os/exec"
	"testing"
	"time"
)

// TestProcAlive verifies the platform process-control primitive that
// stop/down rely on. On Unix it uses signal-0; on Windows it uses
// OpenProcess + GetExitCodeProcess (STILL_ACTIVE). Both implementations must
// agree that a just-started process is alive and an exited one is not — this
// is the contract that makes cross-platform `stunt stop`/`stunt down` work.
func TestProcAlive(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap so liveness reflects reality

	if !procAlive(pid) {
		t.Fatalf("procAlive(%d) = false, want true (process just started)", pid)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill sleep: %v", err)
	}

	if !waitForProcGone(pid, 3*time.Second) {
		t.Fatalf("procAlive(%d) still true after kill", pid)
	}
}

// TestStopPIDStopsRealProcess verifies stopPID terminates a real running
// process via the cross-platform path (graceful stop -> wait -> force kill)
// used by `stunt stop`. On Windows this previously failed with
// "not supported by windows" because os.Process.Signal only honors os.Kill.
func TestStopPIDStopsRealProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()

	if err := stopInstance(io.Discard, pid, "", ""); err != nil {
		t.Fatalf("stopInstance(%d): %v", pid, err)
	}

	if !waitForProcGone(pid, 3*time.Second) {
		t.Fatalf("process %d still alive after stopInstance", pid)
	}
}

// waitForProcGone polls procAlive until it reports the process gone or the
// deadline elapses. Returns true once the process is no longer alive.
func waitForProcGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !procAlive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
