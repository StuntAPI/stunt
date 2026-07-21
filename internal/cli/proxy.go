package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/netutil"
	"stuntapi.com/stunt/internal/netutil/proxy"
)

func newProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Manage the TLS reverse proxy",
	}
	cmd.AddCommand(newProxyStartCmd())
	return cmd
}

func newProxyStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the TLS reverse proxy (auto-elevates for privileged ports)",
		RunE:  runProxyStart,
	}
	cmd.Flags().Int("port", 443, "listen port for the proxy")
	cmd.Flags().Bool("no-tls", false, "disable TLS (plain HTTP)")
	cmd.Flags().Bool("foreground", false, "run in foreground (do not re-exec with sudo)")
	return cmd
}

func runProxyStart(cmd *cobra.Command, args []string) error {
	port, _ := cmd.Flags().GetInt("port")
	noTLS, _ := cmd.Flags().GetBool("no-tls")
	foreground, _ := cmd.Flags().GetBool("foreground")
	manifestPath, _ := cmd.Flags().GetString("manifest")

	addr := fmt.Sprintf(":%d", port)
	useTLS := !noTLS

	// Privileged port + not root → re-exec under sudo (unless --foreground).
	if isPrivilegedPort(port) && !isRoot() && !foreground {
		// Build args preserving the original invocation but forcing
		// --foreground so we don't recurse.
		passthrough := []string{"proxy", "start", "--port", fmt.Sprintf("%d", port), "--foreground"}
		if noTLS {
			passthrough = append(passthrough, "--no-tls")
		}
		passthrough = append(passthrough, "--manifest", manifestPath)
		reexec, err := sudoReexecCmd(passthrough...)
		if err != nil {
			return fmt.Errorf("proxy: construct sudo re-exec: %w", err)
		}
		reexec.Stdin = os.Stdin
		reexec.Stdout = cmd.OutOrStdout()
		reexec.Stderr = cmd.ErrOrStderr()
		fmt.Fprintf(cmd.OutOrStdout(), "port %d requires privilege — re-execing via sudo\n", port)
		return reexec.Run()
	}

	// Ensure CA exists.
	caDir := caPath(manifestDir(manifestPath))
	ca, err := netutil.EnsureCA(caDir)
	if err != nil {
		return fmt.Errorf("proxy: CA: %w", err)
	}

	p, err := proxy.New(proxy.Options{
		TLS:  useTLS,
		Addr: addr,
		CA:   ca,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	proto := "https"
	if !useTLS {
		proto = "http"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "stunt proxy listening on %s://%s\n", proto, addr)
	return p.ListenAndServe(ctx)
}
