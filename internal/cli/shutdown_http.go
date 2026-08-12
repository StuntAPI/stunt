package cli

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// httpGracefulShutdown POSTs to the server's dashboard /api/shutdown endpoint
// to request an in-process graceful shutdown. This is the cross-platform
// graceful path: the endpoint cancels the same context SIGTERM would, so the
// server drains in-flight requests via http.Server.Shutdown.
//
// It returns nil only on a 2xx response. Any other outcome — no URL, a
// transport error, or a non-2xx status — returns an error so the caller can
// escalate to the platform signal/kill path. The 3s client timeout bounds how
// long we wait for the server to acknowledge the request (not how long it
// takes to finish shutting down; that is bounded separately by waitForExit).
func httpGracefulShutdown(dashboardURL, token string) error {
	if dashboardURL == "" {
		return errors.New("no dashboard URL")
	}
	req, err := http.NewRequest(http.MethodPost, dashboardURL+"/api/shutdown", nil)
	if err != nil {
		return fmt.Errorf("build shutdown request: %w", err)
	}
	req.Header.Set("X-Stunt-Token", token)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("shutdown request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("shutdown returned %s", resp.Status)
	}
	return nil
}
