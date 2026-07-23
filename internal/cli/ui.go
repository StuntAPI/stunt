package cli

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
)

// openBrowser is the seam that launches the user's default browser at a URL.
// It is a package-level var so tests can override it with a recorder.
var openBrowser = defaultOpenBrowser

// defaultOpenBrowser picks the platform's "open URL" command and runs it. A
// missing open binary is not fatal (we only warn), since headless / CI
// environments may not have one.
func defaultOpenBrowser(rawURL string) error {
	var openCmd string
	switch runtime.GOOS {
	case "darwin":
		openCmd = "open"
	case "windows":
		// `start` is a cmd.exe builtin, so shell out via cmd /c.
		return exec.Command("cmd", "/c", "start", "", rawURL).Run()
	default:
		openCmd = "xdg-open"
	}
	return exec.Command(openCmd, rawURL).Run()
}

func newUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ui",
		Short: "Open the stunt dashboard in your default browser",
		Long: `Open the stunt dashboard in your default browser.

Resolves the running server's dashboard URL + token from the manifest dir's
runtime file (written by ` + "`stunt up`" + `), then opens
<url>/?token=<token>. The dashboard exchanges that token query param for a
stunt_token cookie (HttpOnly, SameSite=Strict) and redirects, so subsequent
page navigation and the WebSocket stream are authenticated automatically — no
custom header needed in the browser.

If no server is running, run ` + "`stunt up`" + ` first.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			manifestPath, _ := cmd.Flags().GetString("manifest")
			return runUI(manifestPath)
		},
	}
}

// runUI resolves the dashboard URL+token (via resolveDashboard, so it works
// with zero flags against a running `stunt up`), prints the URL + a note, and
// opens the browser to <url>/?token=<token> to bootstrap cookie auth.
func runUI(manifestPath string) error {
	url, token, err := resolveDashboard("", "", manifestPath)
	if err != nil {
		return err
	}
	fmt.Printf("Opening dashboard: %s\n", url)
	fmt.Println("(Authenticating via a one-time token query param → cookie.)")
	if err := openBrowser(url + "/?token=" + token); err != nil {
		// A missing open binary in a headless environment is not fatal.
		fmt.Printf("warning: could not open browser automatically (%v)\n", err)
		fmt.Printf("Open this URL manually: %s/?token=%s\n", url, token)
	}
	return nil
}
