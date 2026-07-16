package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
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
	cmd.Flags().Int("proxy-port", 0, "proxy listen port (subdomain mode only; 0 = OS-assigned high port; use stunt setup/service for :443)")
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
	// M9: defer engineShutdown after the error check so the engine is
	// always closed even on early return.
	defer engineShutdown()

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
	// I5: default to ":0" (OS-assigned high port) instead of ":443" so a
	// non-root user doesn't get a cryptic permission-denied. Using :443
	// requires `stunt setup`/service.
	proxyAddr := ":0"
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
		return err
	}

	// Create the listener ourselves so we can learn the actual port (I5).
	ln, err := net.Listen("tcp", proxyAddr)
	if err != nil {
		return fmt.Errorf("subdomain: listen: %w", err)
	}
	actualPort := portFromListener(ln)

	// M5: if sync_hosts is enabled, write *.tld entries to the hosts file.
	// Uses the hostsPath indirection so tests can override it.
	if m.Network.SyncHosts {
		if err := maybeSyncHosts(out, m, hostsPath, actualPort); err != nil {
			fmt.Fprintf(out, "warning: %v\n", err)
		}
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
		fmt.Fprintf(out, "  %s  ->  %s://%s.%s:%s\n", name, scheme, name, tld, actualPort)
	}
	fmt.Fprintln(out, "stunt up (subdomain mode) — Ctrl-C to stop")

	// Start the proxy (blocks until ctx is canceled).
	return p.Serve(ctx, ln)
}

// maybeSyncHosts writes <service>.<tld> entries to the hosts file pointing
// at 127.0.0.1, using the hostsPath indirection for testability (M5).
func maybeSyncHosts(out interface{ Write([]byte) (int, error) }, m *manifest.Manifest, hostsFile string, port string) error {
	entries := make([]netutil.HostEntry, 0, len(m.Services))
	for name := range m.Services {
		entries = append(entries, netutil.HostEntry{Host: name + "." + m.Network.TLD})
	}
	if err := netutil.SyncHosts(hostsFile, entries); err != nil {
		return fmt.Errorf("hosts sync: %w", err)
	}
	fmt.Fprintf(out, "synced %d host(s) to %s\n", len(entries), hostsFile)
	return nil
}

// portFromListener extracts the port string from a net.Listener.
func portFromListener(ln net.Listener) string {
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		return strconv.Itoa(addr.Port)
	}
	return ""
}
