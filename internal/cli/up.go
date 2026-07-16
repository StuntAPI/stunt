package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/engine"
	"github.com/stunt-adapters/stunt/internal/manifest"
)

func newUpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up",
		Short: "Start all configured services (foreground)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			m, err := manifest.Load(path)
			if err != nil {
				return fmt.Errorf("load %s: %w", path, err)
			}
			if err := manifest.Validate(m); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			port := m.Network.BasePort
			for name, svc := range m.Services {
				if svc.Adapter != "" {
					fmt.Fprintf(out, "  %s  ->  http://127.0.0.1:%d  (adapter: %s, %d rules)\n", name, port, svc.Adapter, len(svc.Rules))
				} else {
					fmt.Fprintf(out, "  %s  ->  http://127.0.0.1:%d  (%d rules)\n", name, port, len(svc.Rules))
				}
				port++
			}
			fmt.Fprintln(out, "stunt up — Ctrl-C to stop")

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			e, err := engine.New(m)
			if err != nil {
				return err
			}
			defer e.Close()
			return e.Serve(ctx)
		},
	}
}
