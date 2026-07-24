package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/adapter"
	"stuntapi.com/stunt/internal/engine"
	"stuntapi.com/stunt/internal/engine/dashboard"
	"stuntapi.com/stunt/internal/engine/requestlog"
	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/netutil"
	"stuntapi.com/stunt/internal/netutil/proxy"
)

func newUpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start all configured services (foreground)",
		Long: `Start all services declared in stunt.yaml, in the foreground, until Ctrl-C.

This is stunt's main command. It loads the manifest, mounts each service's
adapter (if any), opens the listen ports, and serves requests. In subdomain
mode it also starts the TLS reverse proxy so services are reachable at
https://<service>.localhost.

Run "stunt plan" first to validate the manifest and check for port conflicts
without starting servers.`,
		RunE: runUp,
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
		e, err := engine.New(m)
		if err != nil {
			return err
		}
		defer e.Close()
		return runUpPort(ctx, e, m, out)
	}
}

// runUpPort serves each service on its own port. The engine must already be
// created by the caller. Banner addresses come from the engine's actual
// listener ports so they always match what `stunt plan` predicts (both use
// alphabetical service order).
func runUpPort(ctx context.Context, e *engine.Engine, m *manifest.Manifest, out interface{ Write([]byte) (int, error) }) error {
	addrs, cancel, err := e.Start(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	dashURL, dashToken := startDashboard(ctx, e)
	if dashURL != "" {
		fmt.Fprintf(out, "  dashboard:  %s   (token: %s)\n", dashURL, dashToken)
	}

	// Write the runtime file so `stunt down` can stop this server.
	manifestDir := filepath.Dir(m.Path)
	var addrList []string
	for _, name := range sortedServiceNames(m.Services) {
		addrList = append(addrList, addrs[name])
	}
	rt := RuntimeFile{
		PID:            os.Getpid(),
		Manifest:       m.Path,
		Mode:           "port",
		Addresses:      addrList,
		StartedAt:      time.Now().Format(time.RFC3339),
		DashboardURL:   dashURL,
		DashboardToken: dashToken,
	}
	if wErr := writeRuntimeFile(manifestDir, rt); wErr != nil {
		fmt.Fprintf(out, "warning: could not write runtime file: %v\n", wErr)
	}
	defer removeRuntimeFile(manifestDir)

	// Register in the global instance registry (~/.stunt/instances.json) so
	// `stunt ps` can list this server across manifests. Best-effort; a missed
	// deregister (crash) is healed by PID-pruning on the next `stunt ps`.
	if reg, err := OpenRegistry(); err == nil {
		_ = reg.Register(Instance{
			PID:            os.Getpid(),
			Manifest:       m.Path,
			Mode:           "port",
			Services:       sortedServiceNames(m.Services),
			Addresses:      addrList,
			DashboardURL:   dashURL,
			DashboardToken: dashToken,
			StartedAt:      rt.StartedAt,
		})
		defer reg.Deregister(os.Getpid())
	}

	for _, name := range sortedServiceNames(m.Services) {
		svc := m.Services[name]
		httpAddr := addrs[name]
		grpcTarget := e.GrpcTarget(name)

		// Show a clear error for services whose adapter failed to load
		// (partial startup — the service is still reachable but returns 503).
		if loadErr := e.ServiceLoadError(name); loadErr != "" {
			fmt.Fprintf(out, "  %s  ->  %s  (LOAD ERROR: %s)\n", name, httpAddr, loadErr)
			continue
		}

		if svc.Adapter != "" {
			summary := upServiceSummary(svc, e.AdapterFor(name))
			if grpcTarget != "" {
				fmt.Fprintf(out, "  %s  ->  %s  grpc://%s  %s\n", name, httpAddr, grpcTarget, summary)
			} else {
				fmt.Fprintf(out, "  %s  ->  %s  %s\n", name, httpAddr, summary)
			}
		} else {
			fmt.Fprintf(out, "  %s  ->  %s  (%d rules)\n", name, httpAddr, len(svc.Rules))
		}
	}
	fmt.Fprintln(out, "stunt up — Ctrl-C to stop")

	<-ctx.Done()
	// Graceful shutdown: suppress the "context canceled" error that looks
	// like a failure to the user. Print a clean message instead.
	fmt.Fprintln(out, "stopped.")
	return nil
}

// startDashboard runs the localhost admin UI; returns its URL + token. A bind
// failure is non-fatal: it logs a warning and returns "". The dashboard is
// wired with the engine's shared seq counter and an engine-backed ReplayFunc
// that re-issues a captured request in-process through the (unwrapped) service
// handler. Because HandlerForTestByName returns the raw service handler (the
// recorder is only added at the real serving site in serve.go), replay does
// NOT trigger the recorder's Enqueue — so the replayed request is logged
// exactly once, by handleReplay's own manual Enqueue.
func startDashboard(ctx context.Context, e *engine.Engine) (string, string) {
	rl := e.RequestLog()
	if rl == nil {
		return "", ""
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: dashboard disabled: %v\n", err)
		return "", ""
	}
	d := dashboard.New(rl)
	d.SetSeq(e.Seq())
	d.SetReplayFunc(func(ent requestlog.Entry) (int, string) {
		rw := httptest.NewRecorder()
		var body *strings.Reader
		if ent.ReqBody != "" {
			body = strings.NewReader(ent.ReqBody)
		} else {
			body = strings.NewReader("")
		}
		req := httptest.NewRequest(ent.Method, ent.Path, body)
		for k, vs := range decodeHeaders(ent.ReqHeaders) {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		e.HandlerForTestByName(ent.Service).ServeHTTP(rw, req)
		return rw.Code, rw.Body.String()
	})

	// Data browsers + reset (Plan 3): engine-backed providers over per-service stores.
	d.SetServices(e.ServiceNames())
	d.SetState(
		func(svc string) (dashboard.ServiceState, bool) {
			col, k, b, ok := e.StateStores(svc)
			if !ok {
				return dashboard.ServiceState{}, false
			}
			st := dashboard.ServiceState{}
			if col != nil {
				names, _ := col.CollectionNames()
				for _, n := range names {
					c, err := col.Collection(n)
					cnt := 0
					if err == nil {
						cnt, _ = c.Count()
					}
					st.Collections = append(st.Collections, dashboard.CollectionInfo{Name: n, Count: cnt})
				}
			}
			if k != nil {
				st.KVNames, _ = k.Namespaces()
			}
			if b != nil {
				st.BlobNames, _ = b.Namespaces()
			}
			return st, true
		},
		func(svc, name string) ([]map[string]any, error) {
			col, _, _, ok := e.StateStores(svc)
			if !ok {
				return nil, fmt.Errorf("no state for service %s", svc)
			}
			c, err := col.Collection(name)
			if err != nil {
				return nil, err
			}
			return c.List()
		},
		func(svc, ns string) ([][2]string, error) {
			_, k, _, ok := e.StateStores(svc)
			if !ok || k == nil {
				return nil, fmt.Errorf("no kv for service %s", svc)
			}
			return k.List(ns)
		},
		func(svc, ns string) ([]dashboard.BlobInfo, error) {
			_, _, b, ok := e.StateStores(svc)
			if !ok || b == nil {
				return nil, fmt.Errorf("no blobs for service %s", svc)
			}
			var namespaces []string
			if ns != "" {
				namespaces = []string{ns}
			} else {
				namespaces, _ = b.Namespaces()
			}
			var out []dashboard.BlobInfo
			for _, n := range namespaces {
				infos, err := b.List(n)
				if err != nil {
					return nil, err
				}
				for _, info := range infos {
					out = append(out, dashboard.BlobInfo{Name: info.Name, Size: info.Size, ContentType: info.ContentType, Modified: info.Modified.Format(time.RFC3339)})
				}
			}
			return out, nil
		},
		func(svc string) error {
			if svc == "" {
				return e.ResetAll()
			}
			return e.ResetService(svc)
		},
	)
	// Snapshot/restore (Plan 3b): engine-backed archive save/load.
	d.SetSnapshot(
		func(w io.Writer) error { return engine.Snapshot(e, "", w) },
		func(r io.Reader) error { _, err := engine.Restore(e, r); return err },
	)
	// Instances panel (Phase 2): the dashboard lists + stops its siblings via the
	// global registry. Stop reuses the cli stopPID (SIGTERM→SIGKILL).
	if reg, err := OpenRegistry(); err == nil {
		d.SetInstances(
			func() ([]dashboard.InstanceInfo, error) {
				insts, err := reg.List(true)
				if err != nil {
					return nil, err
				}
				out := make([]dashboard.InstanceInfo, len(insts))
				for i, in := range insts {
					out[i] = dashboard.InstanceInfo{
						PID: in.PID, Manifest: in.Manifest, Mode: in.Mode,
						Services: in.Services, Addresses: in.Addresses,
						DashboardURL: in.DashboardURL, DashboardToken: in.DashboardToken,
						StartedAt: in.StartedAt,
					}
				}
				return out, nil
			},
			func(pid int) error { return stopPID(pid) },
		)
	}
	srv := &http.Server{Handler: d.Handler()}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), d.Token()
}

// decodeHeaders parses a stored JSON header blob into a map[string][]string.
// A blank or unparseable blob yields nil. (Mirrors the unexported helper in
// the dashboard package; kept local to up.go to avoid an export.)
func decodeHeaders(s string) map[string][]string {
	if s == "" {
		return nil
	}
	var h map[string][]string
	if err := json.Unmarshal([]byte(s), &h); err != nil {
		return nil
	}
	return h
}

// upServiceSummary renders the parenthesised summary for an adapter-backed
// service in the `stunt up` banner. When the adapter is available it includes
// gRPC method and WebSocket route counts.
func upServiceSummary(svc manifest.Service, a *adapter.Adapter) string {
	if a == nil {
		return fmt.Sprintf("(adapter: %s, %d rules)", svc.Adapter, len(svc.Rules))
	}
	parts := []string{fmt.Sprintf("%d endpoints", len(a.Endpoints))}
	if a.Grpc != nil && len(a.Grpc.Methods) > 0 {
		parts = append(parts, fmt.Sprintf("%d grpc methods", len(a.Grpc.Methods)))
	}
	if len(a.Websockets) > 0 {
		parts = append(parts, fmt.Sprintf("%d ws routes", len(a.Websockets)))
	}
	parts = append(parts, fmt.Sprintf("%d rules", len(svc.Rules)))
	return fmt.Sprintf("(adapter: %s, %s)", svc.Adapter, strings.Join(parts, ", "))
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
	// Manifest tls:false → HTTP; tls:true/omitted → HTTPS (default).
	// The --no-tls CLI flag overrides the manifest to force HTTP.
	useTLS := manifest.ResolveTLS(&m.Network, noTLS)

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

	// Start the dashboard before writing the runtime file so its URL + token
	// can be recorded for auto-discovery (stunt requests / stunt ui).
	subMDir := manifestDir(manifestPath)
	dashURL, dashToken := startDashboard(ctx, e)

	// Write the runtime file so `stunt down` can stop this server.
	subRt := RuntimeFile{
		PID:            os.Getpid(),
		Manifest:       manifestPath,
		Mode:           "subdomain",
		Addresses:      []string{fmt.Sprintf("%s://%s:%s", map[bool]string{true: "https", false: "http"}[useTLS], tld, actualPort)},
		StartedAt:      time.Now().Format(time.RFC3339),
		DashboardURL:   dashURL,
		DashboardToken: dashToken,
	}
	if wErr := writeRuntimeFile(subMDir, subRt); wErr != nil {
		fmt.Fprintf(out, "warning: could not write runtime file: %v\n", wErr)
	}
	defer removeRuntimeFile(subMDir)

	// Register in the global instance registry (~/.stunt/instances.json).
	if reg, err := OpenRegistry(); err == nil {
		_ = reg.Register(Instance{
			PID:            os.Getpid(),
			Manifest:       manifestPath,
			Mode:           "subdomain",
			Services:       sortedServiceNames(m.Services),
			Addresses:      subRt.Addresses,
			DashboardURL:   dashURL,
			DashboardToken: dashToken,
			StartedAt:      subRt.StartedAt,
		})
		defer reg.Deregister(os.Getpid())
	}

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
	if dashURL != "" {
		fmt.Fprintf(out, "  dashboard:  %s   (token: %s)\n", dashURL, dashToken)
	}
	fmt.Fprintln(out, "stunt up (subdomain mode) — Ctrl-C to stop")

	// Start the proxy (blocks until ctx is canceled).
	err = p.Serve(ctx, ln)
	// Graceful shutdown: suppress the "context canceled" message that looks
	// like a failure. Print a clean message instead.
	fmt.Fprintln(out, "stopped.")
	return nil
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
