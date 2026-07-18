package cli

import (
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func newDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop a running stunt server",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			return runDown(cmd.OutOrStdout(), manifestDir(path))
		},
	}
}

// runDown reads the runtime file for the given manifest dir, sends SIGTERM
// to the recorded PID, waits for the process to exit, and removes the
// runtime file. If no server is running, it prints a friendly message.
func runDown(out io.Writer, mDir string) error {
	rt, err := readRuntimeFile(mDir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(out, "no running stunt server found")
			return nil
		}
		return fmt.Errorf("read runtime file: %w", err)
	}

	pid := rt.PID

	// Check if the process exists (signal 0 is a no-op existence check).
	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Unix, os.FindProcess always succeeds; this is for safety.
		removeRuntimeFile(mDir)
		fmt.Fprintf(out, "no running stunt server found (stale runtime file removed)\n")
		return nil
	}

	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process doesn't exist — clean up stale file.
		removeRuntimeFile(mDir)
		fmt.Fprintf(out, "no running stunt server found (stale runtime file removed)\n")
		return nil
	}

	// Send SIGTERM for graceful shutdown.
	fmt.Fprintf(out, "stopping stunt server (pid %d)…\n", pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		removeRuntimeFile(mDir)
		return fmt.Errorf("send SIGTERM to pid %d: %w", pid, err)
	}

	// Wait for the process to exit (poll with timeout).
	if !waitForExit(pid, 5*time.Second) {
		// Escalate to SIGKILL.
		fmt.Fprintf(out, "server did not exit within 5s; sending SIGKILL\n")
		_ = proc.Signal(syscall.SIGKILL)
		waitForExit(pid, 3*time.Second)
		fmt.Fprintf(out, "killed stunt server (pid %d)\n", pid)
	} else {
		fmt.Fprintf(out, "stopped stunt server (pid %d)\n", pid)
	}

	removeRuntimeFile(mDir)
	return nil
}

// waitForExit polls whether the process with the given PID is still running
// (via signal 0) until it exits or the timeout elapses. Returns true if the
// process exited within the timeout.
func waitForExit(pid int, timeout time.Duration) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true // can't find it → treat as exited
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true // process exited
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
