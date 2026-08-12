package cli

import (
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStopEndToEndWithRealBinary builds stunt, starts `stunt up`, then runs
// `stunt stop <pid>` and verifies the server exited and freed its port. This is
// the originally-reported (Windows-broken) command, exercised end-to-end via
// the unified graceful path: the up server has a dashboard, so stopInstance
// POSTs /api/shutdown first (graceful), then would fall back to signals/kill.
func TestStopEndToEndWithRealBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "stunt")
	buildCmd := exec.Command("go", "build", "-o", binary, "./cmd/stunt")
	buildCmd.Dir = repoRoot(t)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build stunt: %v\n%s", err, out)
	}

	mDir := t.TempDir()
	mPath := filepath.Join(mDir, "stunt.yaml")
	manifest := `version: 1
network:
  mode: port
  base_port: 18789
services:
  api:
    rules:
      - match: { method: GET, path: /hello }
        respond: { status: 200 }
`
	if err := os.WriteFile(mPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	upCmd := exec.Command(binary, "up", "--manifest", mPath)
	upCmd.Dir = mDir
	if err := upCmd.Start(); err != nil {
		t.Fatalf("start stunt up: %v", err)
	}

	// Wait for the runtime file (server is up).
	deadline := time.After(6 * time.Second)
	var upRunning bool
	for !upRunning {
		select {
		case <-deadline:
			upCmd.Process.Kill()
			t.Fatalf("timeout waiting for runtime file. state: %v", upCmd.ProcessState)
		case <-time.After(50 * time.Millisecond):
			if _, err := os.Stat(runtimeFilePath(mDir)); err == nil {
				upRunning = true
			}
		}
	}

	// Read the PID from the runtime file written by stunt up.
	rt, err := readRuntimeFile(mDir)
	if err != nil {
		upCmd.Process.Kill()
		t.Fatalf("read runtime file: %v", err)
	}

	// Run `stunt stop <pid>`.
	stopCmd := exec.Command(binary, "stop", strconv.Itoa(rt.PID))
	stopCmd.Dir = mDir
	stopOut, err := stopCmd.CombinedOutput()
	if err != nil {
		upCmd.Process.Kill()
		t.Fatalf("stunt stop failed: %v\n%s", err, stopOut)
	}
	if !strings.Contains(string(stopOut), "stopped") {
		t.Errorf("expected 'stopped' in stop output, got: %s", stopOut)
	}

	// The up process must have exited.
	waitDone := make(chan struct{})
	go func() { _ = upCmd.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
		// good
	case <-time.After(6 * time.Second):
		upCmd.Process.Kill()
		t.Fatal("stunt up process did not exit within 6s after stunt stop")
	}

	// The service port must be free again — the reported bug was the old
	// instance holding port 8000 after a failed stop.
	if len(rt.Addresses) == 0 {
		t.Fatal("no addresses in runtime file")
	}
	// Addresses are full URLs (e.g. "http://127.0.0.1:18789"); pull host:port.
	u, err := url.Parse(rt.Addresses[0])
	if err != nil {
		t.Fatalf("parse address %q: %v", rt.Addresses[0], err)
	}
	ln, err := net.Listen("tcp", u.Host)
	if err != nil {
		t.Fatalf("port %s not freed after stop: %v", u.Host, err)
	}
	ln.Close()
}
