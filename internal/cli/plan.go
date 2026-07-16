package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/manifest"
)

const defaultManifestPath = "stunt.yaml"

func newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Validate the manifest and show what would run",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			m, err := manifest.Load(path)
			if err != nil {
				return fmt.Errorf("load %s: %w", path, err)
			}
			if err := manifest.Validate(m); err != nil {
				return err
			}
			m.Network.Defaults()

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "stunt.yaml OK — %d service(s):\n", len(m.Services))

			switch m.Network.Mode {
			case "subdomain":
				printPlanSubdomain(out, m)
			default:
				printPlanPort(out, m)
			}
			return nil
		},
	}
}

func printPlanPort(out interface{ Write([]byte) (int, error) }, m *manifest.Manifest) {
	port := m.Network.BasePort
	names := sortedServiceNames(m.Services)
	for _, name := range names {
		svc := m.Services[name]
		if svc.Adapter != "" {
			fmt.Fprintf(out, "  %s  ->  127.0.0.1:%d  (adapter: %s, %d rules)\n", name, port, svc.Adapter, len(svc.Rules))
		} else {
			fmt.Fprintf(out, "  %s  ->  127.0.0.1:%d  (%d rules)\n", name, port, len(svc.Rules))
		}
		port++
	}
}

func printPlanSubdomain(out interface{ Write([]byte) (int, error) }, m *manifest.Manifest) {
	tld := m.Network.TLD
	if tld == "" {
		tld = "localhost"
	}
	names := sortedServiceNames(m.Services)
	for _, name := range names {
		svc := m.Services[name]
		if svc.Adapter != "" {
			fmt.Fprintf(out, "  %s  ->  https://%s.%s  (adapter: %s, %d rules)\n", name, name, tld, svc.Adapter, len(svc.Rules))
		} else {
			fmt.Fprintf(out, "  %s  ->  https://%s.%s  (%d rules)\n", name, name, tld, len(svc.Rules))
		}
	}
}

func sortedServiceNames(services map[string]manifest.Service) []string {
	names := make([]string, 0, len(services))
	for n := range services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
