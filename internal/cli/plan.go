package cli

import (
	"fmt"

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
			port := m.Network.BasePort
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "stunt.yaml OK — %d service(s):\n", len(m.Services))
			for name, svc := range m.Services {
				if svc.Adapter != "" {
					fmt.Fprintf(out, "  %s  ->  127.0.0.1:%d  (adapter: %s, %d rules)\n", name, port, svc.Adapter, len(svc.Rules))
				} else {
					fmt.Fprintf(out, "  %s  ->  127.0.0.1:%d  (%d rules)\n", name, port, len(svc.Rules))
				}
				port++
			}
			return nil
		},
	}
}
