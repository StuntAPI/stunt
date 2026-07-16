package cli

import (
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "stunt",
		Short:         "Local API simulators — test against real APIs, locally",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("manifest", defaultManifestPath, "path to stunt.yaml")
	root.AddCommand(newVersionCmd())
	root.AddCommand(newPlanCmd())
	root.AddCommand(newUpCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newProxyCmd())
	root.AddCommand(newTrustCmd())
	root.AddCommand(newHostsCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newCleanCmd())
	root.AddCommand(newServiceCmd())
	root.AddCommand(newAdapterCmd())
	root.AddCommand(newCatalogCmd())
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
