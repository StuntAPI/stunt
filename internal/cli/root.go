package cli

import (
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "stunt",
		Short:         "Local API simulators — test against real APIs, locally",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// Cobra auto-registers a --version flag when Version is set. Use the
	// same output format as the `stunt version` subcommand for consistency.
	root.SetVersionTemplate("stunt {{.Version}}\n")
	root.PersistentFlags().String("manifest", defaultManifestPath, "path to stunt.yaml")
	root.AddCommand(newVersionCmd())
	root.AddCommand(newPlanCmd())
	root.AddCommand(newUpCmd())
	root.AddCommand(newDownCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newProxyCmd())
	root.AddCommand(newTrustCmd())
	root.AddCommand(newHostsCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newCleanCmd())
	root.AddCommand(newServiceCmd())
	root.AddCommand(newAdapterCmd())
	root.AddCommand(newCatalogCmd())
	root.AddCommand(newDemoCmd())
	root.AddCommand(newRequestsCmd())
	root.AddCommand(newReplayCmd())
	root.AddCommand(newStateCmd())
	root.AddCommand(newResetCmd())
	root.AddCommand(newUICmd())
	root.AddCommand(newLLMCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the stunt version",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVersion(cmd.OutOrStdout())
		},
	}
}

func Execute() error {
	return NewRootCmd().Execute()
}
