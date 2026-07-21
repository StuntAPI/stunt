package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/manifest"
)

const sampleManifest = `version: 1
rng_seed: 42
network:
  mode: port
  base_port: 8000
services:
  example:
    rules:
      - name: occasional-error
        match: { method: GET, path: /hello }
        when: { chance: 20 }
        respond: { status: 503, body: { inline: { error: simulated } } }
      - name: success
        match: { method: GET, path: /hello }
        respond: { status: 200, body: { inline: { message: hello from stunt } } }
      - name: slow-timeout
        match: { method: GET, path: /slow }
        when: { chance: 10 }
        respond: { behavior: timeout, latency_ms: 2000 }
      - name: slow-ok
        match: { method: GET, path: /slow }
        respond: { status: 200, body: { inline: { ok: true } }, latency_ms: 100 }
`

func writeSampleManifest(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("refusing to overwrite existing %s", path)
	}
	return os.WriteFile(path, []byte(sampleManifest), 0o644)
}

func loadForInit(path string) (*manifest.Manifest, error) {
	return manifest.Load(path)
}

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create a sample stunt.yaml in the current directory",
		Long: `Create a sample stunt.yaml with an inline, rules-only service demonstrating
stunt's core features: probabilistic faults, templated responses, and
latency simulation.

This is the fastest way to try stunt — run "stunt init", then "stunt up" and
make requests to the served address. No adapter is required for rules-only
services.

To add a stateful adapter service later, see "stunt adapter --help".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			if err := writeSampleManifest(path); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "wrote %s\n", path)
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Next steps:")
			fmt.Fprintln(out, "  stunt plan    # validate and preview")
			fmt.Fprintln(out, "  stunt up      # start serving on http://127.0.0.1:8000")
			return nil
		},
	}
}
