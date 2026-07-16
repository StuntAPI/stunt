package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/contrib"
)

// newAdapterCmd creates the "adapter" parent command group. Future subcommands
// (import, lint, test) are added under here.
func newAdapterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adapter",
		Short: "Manage stunt adapters",
		Long: `Manage stunt adapters — create, validate, and scaffold API simulators.

An adapter is a directory describing how to simulate one API. It contains an
adapter.yaml manifest plus convention directories (endpoints/, templates/,
fixtures/, scripts/, schemas/).`,
	}
	cmd.AddCommand(newAdapterNewCmd())
	return cmd
}

func newAdapterNewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a new adapter skeleton",
		Long: `Create a new adapter skeleton at <dir>/<name>/ (default: ./<name>/).

The skeleton includes adapter.yaml, an example GET /hello endpoint, a sample
template, a seed fixture, a Starlark handler, a JSON schema, and a README.
All data is synthetic.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dir, _ := cmd.Flags().GetString("dir")
			force, _ := cmd.Flags().GetBool("force")
			return runAdapterNew(cmd.OutOrStdout(), dir, name, force)
		},
	}
	cmd.Flags().String("dir", ".", "target directory for the adapter")
	cmd.Flags().Bool("force", false, "overwrite an existing non-empty directory")
	return cmd
}

// runAdapterNew scaffolds an adapter and prints a confirmation message.
func runAdapterNew(out interface{ Write([]byte) (int, error) }, dir, name string, force bool) error {
	opts := contrib.ScaffoldOptions{Force: force}
	if err := contrib.Scaffold(dir, name, opts); err != nil {
		return err
	}
	target := filepath.Join(dir, name)
	fmt.Fprintf(out, "created adapter %s at %s\n", name, target)
	return nil
}
