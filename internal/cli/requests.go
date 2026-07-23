package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"
)

func newRequestsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "List recent requests captured by a running stunt server",
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			token, _ := cmd.Flags().GetString("token")
			manifestPath, _ := cmd.Flags().GetString("manifest")
			limit, _ := cmd.Flags().GetInt("limit")
			asJSON, _ := cmd.Flags().GetBool("json")
			return runRequests(cmd.OutOrStdout(), url, token, manifestPath, asJSON, limit)
		},
	}
	cmd.Flags().String("url", "", "dashboard URL (default: read from the running server)")
	cmd.Flags().String("token", "", "dashboard auth token")
	cmd.Flags().Int("limit", 100, "max entries")
	cmd.Flags().Bool("json", false, "machine-readable JSON output")
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
