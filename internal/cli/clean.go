package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/netutil"
)

func newCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove stunt state, CA files, and hosts block",
		Long: `Remove all stunt-managed state for this manifest: the SQLite-backed adapter
state directory, the local CA files, and stunt's managed block in /etc/hosts.

This resets the simulation to a clean slate — useful when adapter state has grown
or when you want to reseed from fixture data on the next "stunt up".

It does NOT remove the manifest (stunt.yaml), installed adapters, or the
home directory cache (~/.stunt/adapters).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			mdir := manifestDir(path)
			out := cmd.OutOrStdout()
			return runClean(out, mdir, hostsPath)
		},
	}
}

// runClean removes state, CA files, and the hosts managed block. It is
// best-effort: errors are reported but do not abort remaining steps.
// The untrust command for the CA trust store is constructed but NOT
// executed here — it requires privilege. The user can run `stunt untrust`
// or the platform-specific command separately.
func runClean(out interface{ Write([]byte) (int, error) }, mdir, hostsFile string) error {
	// 1. Remove state directory.
	sp := statePath(mdir)
	if _, err := os.Stat(sp); err == nil {
		if err := os.RemoveAll(sp); err != nil {
			fmt.Fprintf(out, "warning: could not remove state dir %s: %v\n", sp, err)
		} else {
			fmt.Fprintf(out, "removed state: %s\n", sp)
		}
	}

	// 2. Remove CA directory (cert files). The trust-store entry requires
	//    privilege and is handled separately via `stunt trust`/untrust.
	caDir := caPath(mdir)
	certPath := filepath.Join(caDir, "ca.pem")
	if fileExistsCLI(certPath) {
		fmt.Fprintf(out, "note: CA trust-store entry is not removed (requires privilege)\n")
		// Construct the untrust command so the user knows what to run.
		if untrust, err := netutil.UntrustCommand(certPath); err == nil {
			fmt.Fprintf(out, "  untrust: %s\n", untrust)
		}
	}
	if _, err := os.Stat(caDir); err == nil {
		if err := os.RemoveAll(caDir); err != nil {
			fmt.Fprintf(out, "warning: could not remove CA dir %s: %v\n", caDir, err)
		} else {
			fmt.Fprintf(out, "removed CA: %s\n", caDir)
		}
	}

	// 3. Clean hosts block.
	hadHosts, _ := netutil.HasManagedBlock(hostsFile)
	if err := netutil.CleanHosts(hostsFile); err != nil {
		fmt.Fprintf(out, "warning: could not clean hosts: %v\n", err)
	} else if hadHosts {
		fmt.Fprintf(out, "cleaned hosts: %s\n", hostsFile)
	}

	return nil
}
