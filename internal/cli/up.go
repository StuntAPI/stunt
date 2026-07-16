package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/engine"
	"github.com/stunt-adapters/stunt/internal/manifest"
	"github.com/stunt-adapters/stunt/internal/netutil"
	"github.com/stunt-adapters/stunt/internal/netutil/proxy"
)

func newUpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start all configured services (foreground)",
		RunE:  runUp,
	}
	cmd.Flags().Int("proxy-port", 0, "proxy listen port (subdomain mode only; 0 = use :443 or OS-assigned)")
	cmd.Flags().Bool("no-tls", false, "disable TLS in subdomain mode")
	return cmd
}

func runUp(cmd *cobra.Command, args []string) error {
	path, _ := cmd.Flags().GetString("manifest")
	m, err := manifest.Load(path)
	if err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	if err := manifest.Validate(m); err != nil {
		return err
	}
	m.Network.Defaults()

	out := cmd.OutOrStdout()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch m.Network.Mode {
	case "subdomain":
		return runUpSubdomain(ctx, cmd, m, out)
	default:
		return runUpPort(ctx, m, out)
	}
}

// runUpPort serves each service on its own port (existing behavior).
func runUpPort(ctx context.Context, m *manifest.Manifest, out interface{ Write([]byte) (int, error) }) error {
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

	e, err := engine.New(m)
	if err != nil {
		return err
	}
	defer e.Close()
	return e.Serve(ctx)
}

// runUpSubdomain serves all services behind a single TLS proxy.
// The engine runs on a free high port and the proxy routes by Host.
func runUpSubdomain(ctx context.Context, cmd *cobra.Command, m *manifest.Manifest, out interface{ Write([]byte) (int, error) }) error {
	proxyPort, _ := cmd.Flags().GetInt("proxy-port")
	noTLS, _ := cmd.Flags().GetBool("no-tls")

	tld := m.Network.TLD
	if tld == "" {
		tld = "localhost"
	}
	useTLS := !noTLS

	manifestPath, _ := cmd.Flags().GetString("manifest")

	// Start the engine on a free high port.
	e, err := engine.New(m)
	if err != nil {
		return err
	}
	defer e.Close()

	engineAddr, engineShutdown, err := e.ServeSingle(ctx, "127.0.0.1:0", tld)
	if err != nil {
		return err
	}
	// Strip the "http://" prefix to get host:port.
	engineBackend := engineAddr
	if len(engineBackend) > 7 && engineBackend[:7] == "http://" {
		engineBackend = engineBackend[7:]
	}

	// Ensure the CA exists.
	caDir := caPath(manifestDir(manifestPath))
	ca, err := netutil.EnsureCA(caDir)
	if err != nil {
		return fmt.Errorf("subdomain: CA: %w", err)
	}

	// Determine proxy address.
	proxyAddr := ":443"
	if proxyPort > 0 {
		proxyAddr = fmt.Sprintf(":%d", proxyPort)
	}

	// Build the proxy with TLS and all service backends.
	backends := make(map[string]string, len(m.Services))
	for name := range m.Services {
		backends[name+"."+tld] = engineBackend
	}

	p, err := proxy.New(proxy.Options{
		TLS:      useTLS,
		Addr:     proxyAddr,
		CA:       ca,
		Backends: backends,
	})
	if err != nil {
		engineShutdown()
		return err
	}

	// Print service URLs.
	scheme := "https"
	if !useTLS {
		scheme = "http"
	}
	names := make([]string, 0, len(m.Services))
	for n := range m.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(out, "  %s  ->  %s://%s.%s\n", name, scheme, name, tld)
	}
	fmt.Fprintln(out, "stunt up (subdomain mode) — Ctrl-C to stop")

	// Start the proxy (blocks until ctx is canceled).
	return p.ListenAndServe(ctx)
}
