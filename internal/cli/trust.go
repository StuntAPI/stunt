package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/netutil"
)

func newTrustCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "trust",
		Short: "Install the stunt CA into the system trust store (requires privilege)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			caDir := caPath(manifestDir(path))
			ca, err := netutil.EnsureCA(caDir)
			if err != nil {
				return fmt.Errorf("trust: %w", err)
			}
			trustCmd, err := netutil.TrustCommand(ca.CertPath)
			if err != nil {
				return fmt.Errorf("trust: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "running: %s\n", trustCmd)
			trustCmd.Stdout = cmd.OutOrStdout()
			trustCmd.Stderr = cmd.ErrOrStderr()
			return trustCmd.Run()
		},
	}
}
