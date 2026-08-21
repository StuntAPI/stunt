package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/spf13/cobra"
)

// Profile state lives in the RUNNING server; these commands talk to its
// dashboard (the same URL+token resolution `stunt requests` uses).

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Activate, list, and inspect behavior profiles on a running server",
		Long: `Activate, list, and inspect behavior profiles on a running server.

A profile is a named behavior mode: either rule bundles declared per service
in stunt.yaml, modes an adapter authors in its adapter.yaml (handlers read
profile_active()), or a global preset that assigns profiles to several
services at once. Activation is runtime-only — it resets when the server
restarts (use ` + "`stunt up --profile <name>`" + ` for a boot default).

Activation by bare name resolves a global preset first; otherwise the name
must be a profile of exactly one service. Errors describe everything that IS
available, so the right name is visible without a second command.`,
	}
	cmd.PersistentFlags().String("url", "", "dashboard URL (default: from the manifest's runtime file)")
	cmd.PersistentFlags().String("token", "", "dashboard token (default: from the manifest's runtime file)")
	cmd.AddCommand(newProfileListCmd(), newProfileShowCmd(), newProfileActivateCmd(), newProfileDeactivateCmd())
	return cmd
}

// profileView mirrors the dashboard's GET /api/profile payload.
type profileView struct {
	ActiveGlobal string            `json:"active_global"`
	Active       map[string]string `json:"active"`
	Catalog      struct {
		Presets []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		} `json:"presets"`
		Services map[string][]struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		} `json:"services"`
	} `json:"catalog"`
}

func newProfileListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show every activatable profile and what is currently active",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			token, _ := cmd.Flags().GetString("token")
			path, _ := cmd.Flags().GetString("manifest")
			asJSON, _ := cmd.Flags().GetBool("json")
			return runProfileList(cmd.OutOrStdout(), url, token, path, asJSON)
		},
	}
	cmd.Flags().Bool("json", false, "machine-readable output")
	return cmd
}

func newProfileShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Describe one profile and where it is active",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			token, _ := cmd.Flags().GetString("token")
			path, _ := cmd.Flags().GetString("manifest")
			return runProfileShow(cmd.OutOrStdout(), url, token, path, args[0])
		},
	}
}

func newProfileActivateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "activate <name>",
		Short: "Activate a profile (global preset, or a name one service defines)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			token, _ := cmd.Flags().GetString("token")
			path, _ := cmd.Flags().GetString("manifest")
			service, _ := cmd.Flags().GetString("service")
			return runProfileActivate(cmd.OutOrStdout(), url, token, path, args[0], service)
		},
	}
	cmd.Flags().String("service", "", "target this service's profile explicitly (disambiguates names defined by several services)")
	return cmd
}

func newProfileDeactivateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deactivate",
		Short: "Deactivate profiles (one service with --service, all without)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			url, _ := cmd.Flags().GetString("url")
			token, _ := cmd.Flags().GetString("token")
			path, _ := cmd.Flags().GetString("manifest")
			service, _ := cmd.Flags().GetString("service")
			return runProfileDeactivate(cmd.OutOrStdout(), url, token, path, service)
		},
	}
	cmd.Flags().String("service", "", "deactivate only this service's profile (default: all)")
	return cmd
}

func runProfileList(out io.Writer, url, token, manifestPath string, asJSON bool) error {
	view, err := fetchProfileView(url, token, manifestPath)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(out).Encode(view)
	}
	if view.ActiveGlobal != "" {
		fmt.Fprintf(out, "active preset: %s\n", view.ActiveGlobal)
	} else if len(view.Active) == 0 {
		fmt.Fprintln(out, "active: none")
	}
	svcs := make([]string, 0, len(view.Catalog.Services))
	for s := range view.Catalog.Services {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	if len(view.Catalog.Presets) > 0 {
		fmt.Fprintln(out, "presets (assign profiles across services):")
		for _, p := range view.Catalog.Presets {
			desc := p.Description
			if desc != "" {
				desc = " — " + desc
			}
			fmt.Fprintf(out, "  %s%s\n", p.Name, desc)
		}
	}
	for _, s := range svcs {
		marker := ""
		if a := view.Active[s]; a != "" {
			marker = fmt.Sprintf("  [active: %s]", a)
		}
		fmt.Fprintf(out, "service %s%s\n", s, marker)
		for _, p := range view.Catalog.Services[s] {
			desc := p.Description
			if desc != "" {
				desc = " — " + desc
			}
			fmt.Fprintf(out, "  %s (%s)%s\n", p.Name, p.Source, desc)
		}
	}
	if len(view.Catalog.Presets) == 0 && len(svcs) == 0 {
		fmt.Fprintln(out, "no profiles defined — declare them per service in stunt.yaml, in an adapter's adapter.yaml, or as a global preset")
	}
	return nil
}

func runProfileShow(out io.Writer, url, token, manifestPath, name string) error {
	view, err := fetchProfileView(url, token, manifestPath)
	if err != nil {
		return err
	}
	found := false
	for _, p := range view.Catalog.Presets {
		if p.Name == name {
			found = true
			fmt.Fprintf(out, "%s — global preset%s\n", name, descSuffix(p.Description))
			if view.ActiveGlobal == name {
				fmt.Fprintln(out, "  active")
			}
		}
	}
	for _, s := range sortedKeys(view.Catalog.Services) {
		for _, p := range view.Catalog.Services[s] {
			if p.Name != name {
				continue
			}
			found = true
			fmt.Fprintf(out, "%s — service %s (%s)%s\n", name, s, p.Source, descSuffix(p.Description))
			if view.Active[s] == name {
				fmt.Fprintf(out, "  active on %s\n", s)
			}
		}
	}
	if !found {
		return fmt.Errorf("no profile %q\nrun `stunt profile list` for everything available", name)
	}
	return nil
}

func runProfileActivate(out io.Writer, url, token, manifestPath, name, service string) error {
	view, err := postProfile(url, token, manifestPath, name, service)
	if err != nil {
		return err
	}
	if service != "" {
		fmt.Fprintf(out, "activated %q on %s\n", name, service)
	} else if view.ActiveGlobal != "" {
		fmt.Fprintf(out, "activated preset %q\n", view.ActiveGlobal)
	} else {
		for _, s := range sortedKeys(view.Active) {
			fmt.Fprintf(out, "activated %q on %s\n", view.Active[s], s)
		}
	}
	return nil
}

func runProfileDeactivate(out io.Writer, url, token, manifestPath, service string) error {
	if _, err := postProfile(url, token, manifestPath, "", service); err != nil {
		return err
	}
	if service != "" {
		fmt.Fprintf(out, "deactivated %s\n", service)
	} else {
		fmt.Fprintln(out, "deactivated all profiles")
	}
	return nil
}

func descSuffix(d string) string {
	if d == "" {
		return ""
	}
	return " — " + d
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fetchProfileView resolves the running server's dashboard and GETs the
// profile state.
func fetchProfileView(url, token, manifestPath string) (profileView, error) {
	var view profileView
	base, tok, err := resolveDashboard(url, token, manifestPath)
	if err != nil {
		return view, err
	}
	body, err := dashboardRequest(http.MethodGet, base+"/api/profile", tok, nil)
	if err != nil {
		return view, err
	}
	if err := json.Unmarshal(body, &view); err != nil {
		return view, fmt.Errorf("decode profile response: %w", err)
	}
	return view, nil
}

// postProfile activates/deactivates and returns the resulting view. A 400
// carries the server's descriptive resolution error verbatim.
func postProfile(url, token, manifestPath, name, service string) (profileView, error) {
	var view profileView
	base, tok, err := resolveDashboard(url, token, manifestPath)
	if err != nil {
		return view, err
	}
	payload, err := json.Marshal(map[string]string{"name": name, "service": service})
	if err != nil {
		return view, err
	}
	body, err := dashboardRequest(http.MethodPost, base+"/api/profile", tok, payload)
	if err != nil {
		return view, err
	}
	if err := json.Unmarshal(body, &view); err != nil {
		return view, fmt.Errorf("decode profile response: %w", err)
	}
	return view, nil
}

// dashboardRequest performs one token-authenticated dashboard call. Non-2xx
// bodies carry {"error": ...} and are returned as errors.
func dashboardRequest(method, url, token string, payload []byte) ([]byte, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Stunt-Token", token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dashboard request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("dashboard returned %s", resp.Status)
	}
	return body, nil
}
