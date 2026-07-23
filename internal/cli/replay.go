package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

func newReplayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replay <id>",
		Short: "Re-issue a captured request against the running stunt server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			token, _ := cmd.Flags().GetString("token")
			manifestPath, _ := cmd.Flags().GetString("manifest")
			asJSON, _ := cmd.Flags().GetBool("json")
			return runReplayAuto(cmd.OutOrStdout(), url, token, manifestPath, args[0], asJSON)
		},
	}
	cmd.Flags().String("url", "", "dashboard URL (default: read from the running server)")
	cmd.Flags().String("token", "", "dashboard auth token")
	cmd.Flags().Bool("json", false, "machine-readable JSON output")
	return cmd
}

// replayResult is the JSON shape returned by POST /api/requests/<id>/replay:
// {"status": <int>, "body": <string | raw json>}.
type replayResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// runReplay re-issues the captured request <id> against the dashboard at url
// (already resolved) and prints the result. It is the test seam for the
// replay command: callers resolve the dashboard URL/token themselves.
func runReplay(out io.Writer, url, token, id string, asJSON bool) error {
	req, err := http.NewRequest(http.MethodPost, url+"/api/requests/"+id+"/replay", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Stunt-Token", token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach dashboard: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("dashboard returned %d for replay of %s", res.StatusCode, id)
	}
	var result replayResult
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	printReplay(out, result, asJSON)
	return nil
}

// runReplayAuto resolves the dashboard from the manifest dir's runtime file
// (unless --url is given) and delegates to runReplay.
func runReplayAuto(out io.Writer, flagURL, flagToken, manifestPath, id string, asJSON bool) error {
	url, token, err := resolveDashboard(flagURL, flagToken, manifestPath)
	if err != nil {
		return err
	}
	return runReplay(out, url, token, id, asJSON)
}

// printReplay writes the replay result either as indented JSON (--json) or as
// a one-line human summary followed by the replayed body.
func printReplay(out io.Writer, r replayResult, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r)
		return
	}
	// The replay endpoint returns status + body (not method/path), so the
	// summary is "replayed -> <status>"; the replayed body follows on the
	// next line so the user can see what came back.
	body := string(r.Body)
	if body == "" {
		body = "(empty)"
	}
	fmt.Fprintf(out, "replayed -> %d\n%s\n", r.Status, body)
}
