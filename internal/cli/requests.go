package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
)

func newRequestsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "List recent requests captured by a running stunt server",
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			token, _ := cmd.Flags().GetString("token")
			manifestPath, _ := cmd.Flags().GetString("manifest")
			limit, _ := cmd.Flags().GetInt("limit")
			asJSON, _ := cmd.Flags().GetBool("json")

			if follow {
				url, token, err := resolveDashboard(url, token, manifestPath)
				if err != nil {
					return err
				}
				// Catch Ctrl-C so the live stream shuts down cleanly.
				ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
				defer cancel()
				return runFollow(ctx, cmd.OutOrStdout(), url, token, asJSON)
			}
			return runRequests(cmd.OutOrStdout(), url, token, manifestPath, asJSON, limit)
		},
	}
	cmd.Flags().String("url", "", "dashboard URL (default: read from the running server)")
	cmd.Flags().String("token", "", "dashboard auth token")
	cmd.Flags().Int("limit", 100, "max entries")
	cmd.Flags().Bool("json", false, "machine-readable JSON output")
	cmd.Flags().BoolVar(&follow, "follow", false, "live-stream requests as they arrive (WebSocket)")
	return cmd
}

// resolveDashboard determines the dashboard URL+token to talk to. Explicit
// --url/--token flags win; otherwise the values are read from the manifest
// dir's runtime file (written by `stunt up`). An empty --url with no runtime
// file yields a friendly error pointing the user at `stunt up`.
func resolveDashboard(flagURL, flagToken, manifestPath string) (string, string, error) {
	if flagURL != "" {
		return flagURL, flagToken, nil
	}
	rt, err := readRuntimeFile(manifestDir(manifestPath))
	if err != nil {
		return "", "", fmt.Errorf("no running stunt server for %s: run `stunt up` (or pass --url/--token): %w", manifestPath, err)
	}
	if rt.DashboardURL == "" {
		return "", "", fmt.Errorf("running server for %s has no dashboard", manifestPath)
	}
	return rt.DashboardURL, rt.DashboardToken, nil
}

func runRequests(out io.Writer, url, token, manifestPath string, asJSON bool, limit int) error {
	url, token, err := resolveDashboard(url, token, manifestPath)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("GET", url+"/api/requests?limit="+strconv.Itoa(limit), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Stunt-Token", token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach dashboard: %w", err)
	}
	defer res.Body.Close()
	var entries []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&entries); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	for _, e := range entries {
		fmt.Fprintf(out, "%4v  %-6v  %-3v  %v\n", e["seq"], e["method"], e["status"], e["path"])
	}
	return nil
}

// runFollow dials the dashboard's WebSocket request stream (ws://<host>
// /api/requests/stream) authenticated with the X-Stunt-Token header, and
// prints each captured request as it arrives. It blocks until ctx is
// cancelled (Ctrl-C) or the server closes the connection.
//
// The dashboard only publishes live events to already-subscribed clients
// (since_seq<=0 yields no backfill), so runFollow is a pure live tail: call it
// once `stunt up` is running to watch requests in real time.
func runFollow(ctx context.Context, out io.Writer, url, token string, asJSON bool) error {
	// The dashboard base URL is http(s)://host; normalize to ws(s):// for the
	// dial. (coder/websocket accepts http(s) schemes too, but we convert for
	// clarity.) Replace https:// before http:// so the inner substring isn't
	// double-rewritten.
	streamURL := url + "/api/requests/stream"
	streamURL = strings.Replace(streamURL, "https://", "wss://", 1)
	streamURL = strings.Replace(streamURL, "http://", "ws://", 1)

	conn, _, err := websocket.Dial(ctx, streamURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Stunt-Token": {token}},
	})
	if err != nil {
		return fmt.Errorf("dial stream: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			// Context cancellation (Ctrl-C / test cancel) is a clean stop, not
			// an error to surface to the user.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read stream: %w", err)
		}
		if asJSON {
			// Re-emit the raw JSON frame on its own line.
			fmt.Fprintln(out, string(msg))
			continue
		}
		// One-line table row; mirror runRequests's table shape.
		var e map[string]any
		if err := json.Unmarshal(msg, &e); err != nil {
			continue // not a JSON object event; skip
		}
		fmt.Fprintf(out, "%4v  %-6v  %-3v  %v\n", e["seq"], e["method"], e["status"], e["path"])
	}
}
