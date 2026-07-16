package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
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
	var servers []*http.Server
	var listeners []net.Listener
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
			return nil, nil, fmt.Errorf("listen for %s: %w", name, err)
		}
		listeners = append(listeners, ln)
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
	}

	cancel := func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(cctx)
		}
	}
	return addrs, cancel, nil
}
