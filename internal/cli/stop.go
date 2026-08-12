package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	progressOut := out
	if asJSON {
		progressOut = io.Discard // keep JSON output clean
	}
	if err := stopInstance(progressOut, pid, found.DashboardURL, found.DashboardToken); err != nil {
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
	progressOut := out
	if asJSON {
		progressOut = io.Discard // keep JSON output clean
	}
	if err := stopInstance(progressOut, found.PID, found.DashboardURL, found.DashboardToken); err != nil {
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

// stopInstance stops a running stunt server, writing progress to out. It
// prefers a graceful dashboard shutdown (POST /api/shutdown — cross-platform,
// lets the server drain in-flight requests), then escalates to a platform
// graceful stop (SIGTERM on Unix; Kill on Windows) and finally a hard kill.
// Used by `stunt stop`, `stunt down`, and the dashboard's instance-stop button.
//
// When dashboardURL is empty (no dashboard available), the HTTP step is
// skipped and the platform signal/kill path is used directly. Returns an error
// only if the process can't be found or isn't running.
func stopInstance(out io.Writer, pid int, dashboardURL, token string) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if !procAlive(pid) {
		return fmt.Errorf("pid %d not running", pid)
	}
	fmt.Fprintf(out, "stopping stunt server (pid %d)…\n", pid)

	// 1. Graceful: ask the server's dashboard to shut down in-process.
	//    This is the only graceful option on Windows (no cross-process
	//    SIGTERM); on Unix it triggers the same path the signal would.
	if dashboardURL != "" {
		if gerr := httpGracefulShutdown(dashboardURL, token); gerr == nil {
			if waitForExit(pid, 5*time.Second) {
				return nil
			}
		}
		// HTTP failed or the server didn't exit in time → fall through.
	}

	// 2. Platform graceful (SIGTERM on Unix; Kill on Windows).
	_ = gracefulStop(proc)
	if !waitForExit(pid, 5*time.Second) {
		// 3. Force.
		fmt.Fprintf(out, "server did not exit within 5s; forcing\n")
		_ = proc.Kill()
		waitForExit(pid, 3*time.Second)
	}
	return nil
}

func absPath(p string) (string, error) {
	return filepath.Abs(p)
}
