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
	root.AddCommand(newVersionCmd())
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
