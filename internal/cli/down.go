package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func newDownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop a running stunt server",
		Long: `Stop a stunt server started in the background (e.g. via the system service).

If you started stunt in the foreground with "stunt up", stop it with Ctrl-C
instead — this command is for background/service-managed instances.`,
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

	// Stale runtime file: the PID isn't alive, so just clean up. procAlive
	// returns false for a gone PID on both Unix (signal-0) and Windows
	// (OpenProcess fails).
	if !procAlive(pid) {
		removeRuntimeFile(mDir)
		fmt.Fprintf(out, "no running stunt server found (stale runtime file removed)\n")
		return nil
	}

	// Stop the server: graceful dashboard shutdown → platform signal → hard
	// kill. stopInstance writes the step progress ("stopping…"/"forcing").
	if err := stopInstance(out, pid, rt.DashboardURL, rt.DashboardToken); err != nil {
		removeRuntimeFile(mDir)
		return err
	}
	fmt.Fprintf(out, "stopped stunt server (pid %d)\n", pid)

	removeRuntimeFile(mDir)

	// Deregister from the global instance registry (best-effort).
	if reg, err := OpenRegistry(); err == nil {
		_ = reg.Deregister(pid)
	}
	return nil
}

// waitForExit polls whether the process with the given PID is still running
// (via the cross-platform procAlive liveness check) until it exits or the
// timeout elapses. Returns true if the process exited within the timeout.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !procAlive(pid) {
			return true // process exited (or can't be opened → treat as exited)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
