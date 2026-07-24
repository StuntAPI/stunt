package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [<pid>]",
		Short: "Stop a running stunt server (by PID or the current manifest's)",
		Long: `Stop a running stunt server.

With a PID argument, stop that instance. With no argument, stop the instance
registered for the current manifest (like stunt down, but resolved via the
global registry). Dead/stale entries are pruned.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			manifestPath, _ := cmd.Flags().GetString("manifest")
			if len(args) == 1 {
				pid, err := strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("stop: %q is not a PID", args[0])
				}
				return runStopPID(cmd.OutOrStdout(), pid, asJSON)
			}
			return runStopManifest(cmd.OutOrStdout(), manifestPath, asJSON)
		},
	}
}

// runStopPID stops a specific instance by PID.
func runStopPID(out io.Writer, pid int, asJSON bool) error {
	reg, err := OpenRegistry()
	if err != nil {
		return err
	}
	insts, err := reg.List(true)
	if err != nil {
		return err
	}
	var found *Instance
	for i := range insts {
		if insts[i].PID == pid {
			found = &insts[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no running stunt server with pid %d", pid)
	}
	if err := stopPID(pid); err != nil {
		return err
	}
	_ = reg.Deregister(pid)
	if asJSON {
		fmt.Fprintf(out, `{"stopped":%d}`+"\n", pid)
	} else {
		fmt.Fprintf(out, "stopped stunt server (pid %d)\n", pid)
	}
	return nil
}

// runStopManifest stops the instance for the given manifest path.
func runStopManifest(out io.Writer, manifestPath string, asJSON bool) error {
	reg, err := OpenRegistry()
	if err != nil {
		return err
	}
	insts, err := reg.List(true)
	if err != nil {
		return err
	}
	abs, _ := absPath(manifestPath)
	var found *Instance
	for i := range insts {
		if insts[i].Manifest == manifestPath || insts[i].Manifest == abs {
			found = &insts[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no running stunt server for %s", manifestPath)
	}
	if err := stopPID(found.PID); err != nil {
		return err
	}
	_ = reg.Deregister(found.PID)
	if asJSON {
		fmt.Fprintf(out, `{"stopped":%d}`+"\n", found.PID)
	} else {
		fmt.Fprintf(out, "stopped stunt server (pid %d)\n", found.PID)
	}
	return nil
}

// stopPID sends SIGTERM (escalating to SIGKILL after 5s) to pid. Mirrors
// stunt down's escalation logic.
func stopPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return fmt.Errorf("pid %d not running: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM to pid %d: %w", pid, err)
	}
	if !waitForExit(pid, 5*time.Second) {
		_ = proc.Signal(syscall.SIGKILL)
		waitForExit(pid, 3*time.Second)
	}
	return nil
}

func absPath(p string) (string, error) {
	return filepath.Abs(p)
}
