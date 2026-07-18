package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ServeForTest starts one server per service on free ports (ignores
// base_port) and returns a map of service name -> http://host:port.
func (e *Engine) ServeForTest(ctx context.Context) (map[string]string, func(), error) {
	return e.serve(ctx, true)
}

// Serve starts one server per service at sequential ports from base_port,
// blocking until ctx is canceled.
func (e *Engine) Serve(ctx context.Context) error {
	_, cancel, err := e.serve(ctx, false)
	if err != nil {
		return err
	}
	<-ctx.Done()
	cancel()
	return ctx.Err()
}

func (e *Engine) serve(ctx context.Context, freePorts bool) (map[string]string, func(), error) {
	names := make([]string, 0, len(e.manifest.Services))
	for n := range e.manifest.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	addrs := make(map[string]string, len(names))
	e.grpcTargets = make(map[string]string)
	var servers []*http.Server
	port := e.manifest.Network.BasePort

	for _, name := range names {
		host := "127.0.0.1"
		listenAddr := fmt.Sprintf("%s:%d", host, port)
		if freePorts {
			listenAddr = host + ":0"
		}
		ln, err := net.Listen("tcp", listenAddr)
		if err != nil {
			for _, s := range servers {
				_ = s.Close()
			}
			for _, gs := range e.grpcServers {
				gs.GracefulStop()
			}
			return nil, nil, fmt.Errorf("listen for %s: %w", name, err)
		}
		svc := e.manifest.Services[name]
		srv := &http.Server{
			Handler:           e.serviceHandler(name, svc),
			ReadHeaderTimeout: 5 * time.Second,
		}
		servers = append(servers, srv)
		go srv.Serve(ln)
		addrs[name] = "http://" + ln.Addr().String()
		if !freePorts {
			port++
		}

		// Start a gRPC server if the adapter declares one.
		if st, ok := e.states[name]; ok && st.adapter != nil && st.adapter.Grpc != nil {
			target, grpcSrv, err := e.startGRPC(ctx, st)
			if err != nil {
				for _, s := range servers {
					_ = s.Close()
				}
				for _, gs := range e.grpcServers {
					gs.GracefulStop()
				}
				return nil, nil, fmt.Errorf("grpc for %s: %w", name, err)
			}
			e.grpcServers = append(e.grpcServers, grpcSrv)
			e.grpcTargets[name] = target
		}
	}

	cancel := func() {
		e.shutdownOnce.Do(func() { close(e.shutdownCh) })
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(cctx)
		}
	}
	return addrs, cancel, nil
}

// ServeSingle starts a single HTTP server (one listener) that dispatches
// all services by Host header, returning the bound address. Used in subdomain
// mode where the TLS proxy fronts the engine. listenAddr may be "127.0.0.1:0"
// for an OS-assigned port.
func (e *Engine) ServeSingle(ctx context.Context, listenAddr, tld string) (string, func(), error) {
	handlers := make(map[string]http.Handler)
	for name, svc := range e.manifest.Services {
		handlers[name] = e.serviceHandler(name, svc)
	}

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := serviceFromHost(r.Host, tld)
		h, ok := handlers[name]
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no service for host %q", name))
			return
		}
		h.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", nil, fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: root, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		e.shutdownOnce.Do(func() { close(e.shutdownCh) })
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(cctx)
	}()

	go srv.Serve(ln)

	addr := "http://" + ln.Addr().String()
	cancel := func() {
		e.shutdownOnce.Do(func() { close(e.shutdownCh) })
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(cctx)
	}
	return addr, cancel, nil
}

// serviceFromHost extracts the service name from a Host header value by
// stripping the TLD suffix. "myapp.localhost" with tld "localhost" -> "myapp".
// Port suffix is stripped first. If the host does not end with the TLD, the
// first label (before the first dot) is used as a fallback.
func serviceFromHost(host, tld string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	// Strip :port if present.
	if hostname, _, err := net.SplitHostPort(h); err == nil {
		h = hostname
	}
	if tld != "" {
		suffix := "." + strings.ToLower(tld)
		if strings.HasSuffix(h, suffix) {
			return strings.TrimSuffix(h, suffix)
		}
	}
	// Fallback: first label before the first dot.
	if idx := strings.Index(h, "."); idx > 0 {
		return h[:idx]
	}
	return h
}
