package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRuntimeFileWriteRead verifies the lifecycle file can be written and
// read back.
func TestRuntimeFileWriteRead(t *testing.T) {
	dir := t.TempDir()
	rt := RuntimeFile{
		PID:       12345,
		Manifest:  filepath.Join(dir, "stunt.yaml"),
		Mode:      "port",
		Addresses: []string{"127.0.0.1:8000"},
		StartedAt: "2024-01-01T00:00:00Z",
	}
	if err := writeRuntimeFile(dir, rt); err != nil {
		t.Fatalf("writeRuntimeFile: %v", err)
	}

	// Verify the file exists.
	if _, err := os.Stat(runtimeFilePath(dir)); err != nil {
		t.Fatalf("runtime file should exist: %v", err)
	}

	loaded, err := readRuntimeFile(dir)
	if err != nil {
		t.Fatalf("readRuntimeFile: %v", err)
	}
	if loaded.PID != rt.PID {
		t.Errorf("PID = %d, want %d", loaded.PID, rt.PID)
	}
	if loaded.Mode != rt.Mode {
		t.Errorf("Mode = %q, want %q", loaded.Mode, rt.Mode)
	}

	// Remove.
	removeRuntimeFile(dir)
	if _, err := os.Stat(runtimeFilePath(dir)); !os.IsNotExist(err) {
		t.Errorf("runtime file should be gone after remove, got err: %v", err)
	}
}

// TestRunDownNoServer verifies that `stunt down` prints a friendly message
// when no runtime file exists.
func TestRunDownNoServer(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := runDown(&buf, dir); err != nil {
		t.Fatalf("runDown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "no running stunt server found") {
		t.Errorf("expected 'no running stunt server found', got: %s", out)
	}
}

// TestRunDownStaleRuntimeFile verifies that a stale runtime file (pointing
// to a dead PID) is cleaned up.
func TestRunDownStaleRuntimeFile(t *testing.T) {
	dir := t.TempDir()
	// Use a PID that definitely doesn't exist (very high number).
	rt := RuntimeFile{PID: 999999, Mode: "port"}
	if err := writeRuntimeFile(dir, rt); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runDown(&buf, dir); err != nil {
		t.Fatalf("runDown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "stale runtime file removed") {
		t.Errorf("expected 'stale runtime file removed', got: %s", out)
	}
	// File should be gone.
	if _, err := os.Stat(runtimeFilePath(dir)); !os.IsNotExist(err) {
		t.Errorf("runtime file should be removed, got err: %v", err)
	}
}

// TestRunDownStopsRunningProcess verifies that `stunt down` sends SIGTERM
// to a real running process and cleans up the runtime file.
func TestRunDownStopsRunningProcess(t *testing.T) {
	// Start a dummy process that sleeps for a long time.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid

	// Reap the process in a goroutine so it doesn't become a zombie
	// that signal(0) still reports as alive.
	waitCh := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waitCh)
	}()

	dir := t.TempDir()
	rt := RuntimeFile{PID: pid, Mode: "port"}
	if err := writeRuntimeFile(dir, rt); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := runDown(&buf, dir); err != nil {
		t.Fatalf("runDown: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "stopped") {
		t.Errorf("expected 'stopped', got: %s", out)
	}

	// Wait for the process to be fully reaped.
	<-waitCh

	// Runtime file should be gone.
	if _, err := os.Stat(runtimeFilePath(dir)); !os.IsNotExist(err) {
		t.Errorf("runtime file should be removed, got err: %v", err)
	}
}

// TestDownEndToEndWithRealBinary is the full lifecycle integration test:
// build the stunt binary, start `stunt up` in the background in a temp dir,
// then run `stunt down` and verify the process exited and the runtime file
// is gone.
func TestDownEndToEndWithRealBinary(t *testing.T) {
	// Build the stunt binary.
	binary := filepath.Join(t.TempDir(), "stunt")
	buildCmd := exec.Command("go", "build", "-o", binary, "./cmd/stunt")
	buildCmd.Dir = repoRoot(t)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build stunt: %v\n%s", err, out)
	}

	// Create a manifest with a rules-only service (no adapter needed).
	mDir := t.TempDir()
	mPath := filepath.Join(mDir, "stunt.yaml")
	manifest := `version: 1
network:
  mode: port
  base_port: 18765
services:
  api:
    rules:
      - match: { method: GET, path: /hello }
        respond: { status: 200 }
`
	if err := os.WriteFile(mPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start `stunt up` in the background.
	upCmd := exec.Command(binary, "up", "--manifest", mPath)
	upCmd.Dir = mDir
	if err := upCmd.Start(); err != nil {
		t.Fatalf("start stunt up: %v", err)
	}

	// Wait for the runtime file to appear (indicates the server is up).
	deadline := time.After(5 * time.Second)
	var upRunning bool
	for {
		select {
		case <-deadline:
			// Kill the process if it started but didn't write the file.
			upCmd.Process.Kill()
			t.Fatalf("timeout waiting for runtime file. Process state: %v", upCmd.ProcessState)
		case <-time.After(50 * time.Millisecond):
			if _, err := os.Stat(runtimeFilePath(mDir)); err == nil {
				upRunning = true
			}
		}
		if upRunning {
			break
		}
	}

	// Ensure the process is still alive.
	if err := upCmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("stunt up process not running after file write: %v", err)
	}

	// Run `stunt down`.
	downCmd := exec.Command(binary, "down", "--manifest", mPath)
	downCmd.Dir = mDir
	downOut, err := downCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stunt down failed: %v\n%s", err, downOut)
	}

	// Verify output mentions stopping.
	if !strings.Contains(string(downOut), "stopping") {
		t.Errorf("expected 'stopping' in down output, got: %s", downOut)
	}

	// Wait for the up process to exit.
	waitDone := make(chan struct{})
	go func() {
		_ = upCmd.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		// Good — process exited.
	case <-time.After(5 * time.Second):
		upCmd.Process.Kill()
		t.Fatal("stunt up process did not exit within 5s after stunt down")
	}

	// Runtime file should be gone.
	if _, err := os.Stat(runtimeFilePath(mDir)); !os.IsNotExist(err) {
		t.Errorf("runtime file should be gone after down, got err: %v", err)
	}
}
