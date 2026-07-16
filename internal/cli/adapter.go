package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/contrib"
	"github.com/stunt-adapters/stunt/internal/contrib/har"
	"github.com/stunt-adapters/stunt/internal/contrib/openapi"
)

// newAdapterCmd creates the "adapter" parent command group. Subcommands:
// new (scaffold), import (openapi, har).
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
	cmd.AddCommand(newAdapterImportCmd())
	return cmd
}

func newAdapterImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import API specs into an adapter",
		Long: `Import external API specifications (OpenAPI, HAR) into an existing stunt
adapter. All imported data is synthesized — no real API data is copied.`,
	}
	cmd.PersistentFlags().String("dir", ".", "adapter directory to import into")
	cmd.AddCommand(newAdapterImportOpenapiCmd())
	cmd.AddCommand(newAdapterImportHarCmd())
	return cmd
}

func newAdapterImportOpenapiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "openapi <spec.yaml|json>",
		Short: "Import an OpenAPI 3.x spec",
		Long: `Import an OpenAPI 3.x specification (JSON or YAML). For each operation a
synthetic endpoint and template are generated. Response bodies use faker
expressions — no real API data is included.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			return runImportOpenapi(cmd.OutOrStdout(), args[0], dir)
		},
	}
}

func newAdapterImportHarCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "har <file.json>",
		Short: "Import a HAR 1.2 file",
		Long: `Import a HAR 1.2 file. For each unique request method + path an endpoint is
inferred and a synthetic fixture is generated. Real response values are
replaced with faker expressions — no real data is copied.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, _ := cmd.Flags().GetString("dir")
			return runImportHar(cmd.OutOrStdout(), args[0], dir)
		},
	}
}

func runImportOpenapi(out interface{ Write([]byte) (int, error) }, specPath, dir string) error {
	data, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("read spec %s: %w", specPath, err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	if err := openapi.Import(data, absDir); err != nil {
		return err
	}
	fmt.Fprintf(out, "imported OpenAPI spec into %s\n", absDir)
	return nil
}

func runImportHar(out interface{ Write([]byte) (int, error) }, harPath, dir string) error {
	data, err := os.ReadFile(harPath)
	if err != nil {
		return fmt.Errorf("read HAR %s: %w", harPath, err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	if err := har.Import(data, absDir); err != nil {
		return err
	}
	fmt.Fprintf(out, "imported HAR file into %s\n", absDir)
	return nil
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
