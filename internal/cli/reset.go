package cli

import (
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

// runReset wipes one service (svc != "") or all (all=true / svc=="").
func runReset(out io.Writer, url, token, svc string, all bool) error {
	path := "/api/state/reset"
	if !all && svc != "" {
		path = "/api/state/" + svc + "/reset"
	}
	req, _ := http.NewRequest("POST", url+path, nil)
	req.Header.Set("X-Stunt-Token", token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach dashboard: %w", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return fmt.Errorf("dashboard returned %d: %s", res.StatusCode, string(b))
	}
	target := "all services"
	if !all && svc != "" {
		target = svc
	}
	fmt.Fprintf(out, "reset %s\n", target)
	return nil
}

// firstArg returns args[0] or "".
func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// resetCmd wipes simulator state via the running server's dashboard API.
func newResetCmd() *cobra.Command {
	var manifestFlag, urlFlag, tokenFlag string
	var all bool
	cmd := &cobra.Command{
		Use:   "reset [<service>]",
		Short: "Wipe simulator state (one service, or --all) of a running server",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token, err := resolveDashboard(urlFlag, tokenFlag, manifestFlag)
			if err != nil {
				return err
			}
			return runReset(cmd.OutOrStdout(), url, token, firstArg(args), all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "reset all services + the request log")
	cmd.Flags().StringVar(&manifestFlag, "manifest", "stunt.yaml", "manifest path (to resolve the running server)")
	cmd.Flags().StringVar(&urlFlag, "url", "", "dashboard URL (default: read from the running server)")
	cmd.Flags().StringVar(&tokenFlag, "token", "", "dashboard auth token")
	return cmd
}
