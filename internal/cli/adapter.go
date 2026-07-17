package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/contrib"
	"github.com/stunt-adapters/stunt/internal/contrib/conform"
	"github.com/stunt-adapters/stunt/internal/contrib/har"
	"github.com/stunt-adapters/stunt/internal/contrib/lint"
	"github.com/stunt-adapters/stunt/internal/contrib/openapi"
)

// newAdapterCmd creates the "adapter" parent command group. Subcommands:
// new (scaffold), import (openapi, har), lint, test, add, remove, list, update.
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
	cmd.AddCommand(newAdapterLintCmd())
	cmd.AddCommand(newAdapterTestCmd())
	cmd.AddCommand(newAdapterAddCmd())
	cmd.AddCommand(newAdapterRemoveCmd())
	cmd.AddCommand(newAdapterListCmd())
	cmd.AddCommand(newAdapterUpdateCmd())
	// --cache-dir is shared by add/list/update. The env STUNT_ADAPTER_CACHE
	// provides the same override without a flag.
	cmd.PersistentFlags().String("cache-dir", "", "adapter cache directory (env: STUNT_ADAPTER_CACHE, default: ~/.stunt/adapters)")
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

func newAdapterLintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lint [dir]",
		Short: "Scan adapter fixtures and templates for real (non-synthetic) data",
		Long: `Lint scans an adapter's fixtures/ and templates/ for content that looks like
real recorded data rather than synthetic data: real-looking emails, UUIDs,
provider-style IDs (cus_, ch_, …), credit-card numbers, long base64 blobs,
and PII field names with literal values.

Template placeholders ({{ faker.Email }}, {{ uuid }}) are recognized and NOT
flagged. The command exits non-zero if any error-severity finding is
detected, so it can be used as a pre-commit/CI guard.

If no directory is given, the current directory is used.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			return runAdapterLint(cmd.OutOrStdout(), dir)
		},
	}
	return cmd
}

func newAdapterTestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "test [dir]",
		Short: "Replay local traces against the adapter and check conformance",
		Long: `Test replays recorded real traces against a locally-running instance of the
adapter and compares the simulator's responses to the recorded expected
responses. By default only the JSON structure (keys, nesting, types) is
compared — values may differ since the simulator produces synthetic data.
Use --strict to compare exact values.

The traces file is a JSONL of {"request":{method,path,headers,body},
"response":{status,body}} pairs from your own real session. The traces stay
local — they are NOT added to the adapter.

If no directory is given, the current directory is used. If --traces is not
given, the default is traces.jsonl in the adapter directory.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			traces, _ := cmd.Flags().GetString("traces")
			strict, _ := cmd.Flags().GetBool("strict")
			return runAdapterTest(cmd.OutOrStdout(), dir, traces, strict)
		},
	}
	cmd.Flags().String("traces", "", "path to traces JSONL file (default: <dir>/traces.jsonl)")
	cmd.Flags().Bool("strict", false, "compare exact values instead of structure")
	return cmd
}

// runAdapterLint scans an adapter dir for real data and prints findings.
// Returns a non-nil error (with a clear message) when error-severity findings
// are detected, so the CLI exits non-zero.
func runAdapterLint(out interface{ Write([]byte) (int, error) }, dir string) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	findings, err := lint.Lint(absDir)
	if err != nil {
		return err
	}
	for _, f := range findings {
		fmt.Fprintf(out, "  %s  %s:%d  %s\n", f.Severity, f.File, f.Line, f.Message)
	}
	switch {
	case len(findings) == 0:
		fmt.Fprintf(out, "no findings — adapter fixtures and templates look synthetic\n")
	case lint.ExitCode(findings) != 0:
		fmt.Fprintf(out, "\n%d finding(s): real data detected — replace with faker placeholders\n", len(findings))
		return fmt.Errorf("lint found real data — fix the errors above before committing")
	default:
		fmt.Fprintf(out, "\n%d warning(s) — review to confirm data is synthetic\n", len(findings))
	}
	return nil
}

// runAdapterTest replays traces against the adapter and prints a conformance
// report with a score.
func runAdapterTest(out interface{ Write([]byte) (int, error) }, dir, tracesPath string, strict bool) error {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	if tracesPath == "" {
		tracesPath = filepath.Join(absDir, "traces.jsonl")
	}

	report, err := conform.Run(context.Background(), absDir, tracesPath, conform.Options{Strict: strict})
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "conformance: %d/%d traces matched (%.0f%%)\n", report.Matched, report.Total, report.Score())
	for _, m := range report.Mismatched {
		fmt.Fprintf(out, "  MISMATCH %s\n    expected: %s\n    got:      %s\n    reason:   %s\n",
			m.Request, m.Expected, m.Got, m.Reason)
	}
	return nil
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
