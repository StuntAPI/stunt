package cli

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/netutil"
)

// hostsPath is the hosts file to operate on. Defaults to the system hosts
// file; tests override it with a temp file path.
var hostsPath = netutil.DefaultHostsFile

func newHostsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hosts",
		Short: "Manage /etc/hosts entries for stunt services",
		Long: `Manage the /etc/hosts entries stunt uses for subdomain-mode TLS services.

In subdomain mode, stunt maps each service to a <service>.localhost hostname
served over real TLS on port 443. "hosts sync" adds those entries to /etc/hosts
(requires privilege); "hosts clean" removes stunt's managed block.

These are managed as a single delimited block so stunt never touches your
existing hosts entries.`,
	}
	cmd.AddCommand(newHostsSyncCmd(), newHostsCleanCmd())
	return cmd
}

func newHostsSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Add <service>.localhost entries to the hosts file",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			m, err := manifest.Load(path)
			if err != nil {
				return fmt.Errorf("load %s: %w", path, err)
			}
			m.Network.Defaults()
			return runHostsSync(cmd.OutOrStdout(), m, hostsPath)
		},
	}
}

func newHostsCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove stunt's managed block from the hosts file",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHostsClean(cmd.OutOrStdout(), hostsPath)
		},
	}
}

// runHostsSync collects all hostnames from the manifest services and writes
// them to the hosts file as 127.0.0.1 entries inside the managed block.
func runHostsSync(out io.Writer, m *manifest.Manifest, path string) error {
	tld := m.Network.TLD
	if tld == "" {
		tld = "localhost"
	}
	entries := make([]netutil.HostEntry, 0, len(m.Services))
	for name := range m.Services {
		entries = append(entries, netutil.HostEntry{Host: name + "." + tld})
	}
	if err := netutil.SyncHosts(path, entries); err != nil {
		return fmt.Errorf("hosts sync: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Host)
	}
	sort.Strings(names)
	fmt.Fprintf(out, "synced %d host(s) to %s\n", len(names), path)
	return nil
}

// runHostsClean removes the stunt-managed block from the hosts file.
func runHostsClean(out io.Writer, path string) error {
	if err := netutil.CleanHosts(path); err != nil {
		return fmt.Errorf("hosts clean: %w", err)
	}
	fmt.Fprintf(out, "cleaned stunt hosts block from %s\n", path)
	return nil
}
