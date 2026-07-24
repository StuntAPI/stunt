package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"
)

func newPsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List running stunt servers",
		Long: `List running stunt servers across all manifests (from ~/.stunt/instances.json).

Dead processes are pruned automatically. Use --json for machine-readable output.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPs(cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable JSON output")
	return cmd
}

// runPs lists registered instances (pruning dead PIDs first).
func runPs(out io.Writer, asJSON bool) error {
	reg, err := OpenRegistry()
	if err != nil {
		return err
	}
	insts, err := reg.List(true) // prune dead
	if err != nil {
		return err
	}
	if len(insts) == 0 {
		if asJSON {
			fmt.Fprintln(out, "[]")
		} else {
			fmt.Fprintln(out, "no running stunt servers")
		}
		return nil
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(insts)
	}
	fmt.Fprintf(out, "%-8s  %-7s  %-8s  %-20s  %-10s  %s\n", "PID", "MODE", "AGE", "MANIFEST", "DASHBOARD", "SERVICES")
	for _, i := range insts {
		manifest := i.Manifest
		// Shorten the manifest path for the table.
		if dir, label := shortenManifest(i.Manifest); label != "" {
			manifest = dir + "/" + label
		}
		dash := "-"
		if i.DashboardURL != "" {
			dash = i.DashboardURL
		}
		svcs := "-"
		if len(i.Services) > 0 {
			svcs = strconv.Itoa(len(i.Services))
		}
		fmt.Fprintf(out, "%-8d  %-7s  %-8s  %-20s  %-10s  %s\n",
			i.PID, i.Mode, age(i.StartedAt), manifest, dash, svcs)
	}
	return nil
}

// shortenManifest returns a short dir + filename for the manifest path.
func shortenManifest(p string) (string, string) {
	if p == "" {
		return "", ""
	}
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			dir, label := p[:i], p[i+1:]
			// trim further dir if too long
			if j := lastIndexByte(dir, '/'); j >= 0 {
				dir = dir[j+1:]
			}
			return dir, label
		}
	}
	return "", p
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
