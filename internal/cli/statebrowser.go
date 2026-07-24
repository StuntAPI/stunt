package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
)

// stateCmd groups the read-only data-browser subcommands.
func newStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Browse simulator state (collections, kv, blobs) of a running server",
	}
	cmd.AddCommand(newStateCollectionsCmd())
	cmd.AddCommand(newStateCollectionCmd())
	cmd.AddCommand(newStateKVCmd())
	cmd.AddCommand(newStateBlobsCmd())
	return cmd
}

func stateClient(url, token, path string) ([]byte, int, error) {
	req, _ := http.NewRequest("GET", url+path, nil)
	req.Header.Set("X-Stunt-Token", token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("reach dashboard: %w", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return b, res.StatusCode, nil
}

func emit(out io.Writer, b []byte, asJSON bool, pretty bool) {
	if asJSON || pretty {
		var v any
		if json.Unmarshal(b, &v) == nil {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			_ = enc.Encode(v)
			return
		}
	}
	fmt.Fprint(out, string(b))
}

// runStateCollections lists a service's collections (+ counts).
func runStateCollections(out io.Writer, url, token, svc string, asJSON bool) error {
	b, code, err := stateClient(url, token, "/api/state/"+svc+"/collections")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("dashboard returned %d: %s", code, string(b))
	}
	emit(out, b, asJSON, false)
	return nil
}

func newStateCollectionsCmd() *cobra.Command {
	var manifestFlag, urlFlag, tokenFlag string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "collections",
		Short: "List a service's collections (+ counts)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := args[0]
			url, token, err := resolveDashboard(urlFlag, tokenFlag, manifestFlag)
			if err != nil {
				return err
			}
			return runStateCollections(cmd.OutOrStdout(), url, token, svc, asJSON)
		},
	}
	addStateFlags(cmd, &manifestFlag, &urlFlag, &tokenFlag, &asJSON)
	return cmd
}

func newStateCollectionCmd() *cobra.Command {
	var manifestFlag, urlFlag, tokenFlag string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "collection <service> <name>",
		Short: "List the documents in a collection",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token, err := resolveDashboard(urlFlag, tokenFlag, manifestFlag)
			if err != nil {
				return err
			}
			b, code, err := stateClient(url, token, "/api/state/"+args[0]+"/collections/"+args[1])
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("dashboard returned %d: %s", code, string(b))
			}
			emit(cmd.OutOrStdout(), b, asJSON, true)
			return nil
		},
	}
	addStateFlags(cmd, &manifestFlag, &urlFlag, &tokenFlag, &asJSON)
	return cmd
}

func newStateKVCmd() *cobra.Command {
	var manifestFlag, urlFlag, tokenFlag string
	var asJSON bool
	var ns string
	cmd := &cobra.Command{
		Use:   "kv <service>",
		Short: "List kv namespaces, or pairs in --ns",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token, err := resolveDashboard(urlFlag, tokenFlag, manifestFlag)
			if err != nil {
				return err
			}
			path := "/api/state/" + args[0] + "/kv"
			if ns != "" {
				path = "/api/state/" + args[0] + "/kv/" + ns
			}
			b, code, err := stateClient(url, token, path)
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("dashboard returned %d: %s", code, string(b))
			}
			emit(cmd.OutOrStdout(), b, asJSON, ns != "")
			return nil
		},
	}
	addStateFlags(cmd, &manifestFlag, &urlFlag, &tokenFlag, &asJSON)
	cmd.Flags().StringVar(&ns, "ns", "", "namespace (lists pairs instead of namespaces)")
	return cmd
}

func newStateBlobsCmd() *cobra.Command {
	var manifestFlag, urlFlag, tokenFlag string
	var asJSON bool
	var ns string
	cmd := &cobra.Command{
		Use:   "blobs <service>",
		Short: "List blobs (optionally in --ns)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token, err := resolveDashboard(urlFlag, tokenFlag, manifestFlag)
			if err != nil {
				return err
			}
			path := "/api/state/" + args[0] + "/blobs"
			if ns != "" {
				path += "?ns=" + ns
			}
			b, code, err := stateClient(url, token, path)
			if err != nil {
				return err
			}
			if code != 200 {
				return fmt.Errorf("dashboard returned %d: %s", code, string(b))
			}
			emit(cmd.OutOrStdout(), b, asJSON, true)
			return nil
		},
	}
	addStateFlags(cmd, &manifestFlag, &urlFlag, &tokenFlag, &asJSON)
	cmd.Flags().StringVar(&ns, "ns", "", "namespace")
	return cmd
}

// addStateFlags wires the common --json/--manifest/--url/--token flags.
func addStateFlags(cmd *cobra.Command, manifestFlag, urlFlag, tokenFlag *string, asJSON *bool) {
	cmd.Flags().BoolVar(asJSON, "json", false, "machine-readable JSON output")
	cmd.Flags().StringVar(manifestFlag, "manifest", "stunt.yaml", "manifest path (to resolve the running server)")
	cmd.Flags().StringVar(urlFlag, "url", "", "dashboard URL (default: read from the running server)")
	cmd.Flags().StringVar(tokenFlag, "token", "", "dashboard auth token")
}
