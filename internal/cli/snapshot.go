package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func newSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Save or restore a snapshot of simulator state",
	}
	cmd.AddCommand(newSnapshotSaveCmd())
	cmd.AddCommand(newSnapshotLoadCmd())
	return cmd
}

func newSnapshotSaveCmd() *cobra.Command {
	var manifestFlag, urlFlag, tokenFlag, outFlag string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "save [-o file]",
		Short: "Download a snapshot archive of the running server's state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token, err := resolveDashboard(urlFlag, tokenFlag, manifestFlag)
			if err != nil {
				return err
			}
			if outFlag == "" {
				outFlag = "stunt-snapshot-" + time.Now().UTC().Format("20060102-150405") + ".tar.gz"
			}
			return runSnapshotSave(cmd.OutOrStdout(), url, token, outFlag, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable JSON output")
	cmd.Flags().StringVarP(&outFlag, "out", "o", "", "output file (default: stunt-snapshot-<timestamp>.tar.gz)")
	addDashFlags(cmd, &manifestFlag, &urlFlag, &tokenFlag)
	return cmd
}

// runSnapshotSave downloads the snapshot archive to outPath.
func runSnapshotSave(out io.Writer, url, token, outPath string, asJSON bool) error {
	req, _ := http.NewRequest("GET", url+"/api/state/snapshot", nil)
	req.Header.Set("X-Stunt-Token", token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach dashboard: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("dashboard returned %d: %s", res.StatusCode, string(b))
	}
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	n, err := io.Copy(f, res.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"path": outPath, "bytes": n})
	}
	fmt.Fprintf(out, "saved %s (%d bytes)\n", outPath, n)
	return nil
}

func newSnapshotLoadCmd() *cobra.Command {
	var manifestFlag, urlFlag, tokenFlag string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "load <file>",
		Short: "Restore simulator state from a snapshot archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url, token, err := resolveDashboard(urlFlag, tokenFlag, manifestFlag)
			if err != nil {
				return err
			}
			return runSnapshotLoad(cmd.OutOrStdout(), url, token, args[0], asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable JSON output")
	addDashFlags(cmd, &manifestFlag, &urlFlag, &tokenFlag)
	return cmd
}

// runSnapshotLoad uploads the archive file to the restore endpoint.
func runSnapshotLoad(out io.Writer, url, token, path string, asJSON bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	req, _ := http.NewRequest("POST", url+"/api/state/restore", bytes.NewReader(data))
	req.Header.Set("X-Stunt-Token", token)
	req.Header.Set("Content-Type", "application/gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach dashboard: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		return fmt.Errorf("dashboard returned %d: %s", res.StatusCode, string(body))
	}
	if asJSON {
		fmt.Fprint(out, string(body))
		return nil
	}
	fmt.Fprintf(out, "restored from %s\n", path)
	return nil
}

// addDashFlags wires the common --manifest/--url/--token flags.
func addDashFlags(cmd *cobra.Command, manifestFlag, urlFlag, tokenFlag *string) {
	cmd.Flags().StringVar(manifestFlag, "manifest", "stunt.yaml", "manifest path (to resolve the running server)")
	cmd.Flags().StringVar(urlFlag, "url", "", "dashboard URL (default: read from the running server)")
	cmd.Flags().StringVar(tokenFlag, "token", "", "dashboard auth token")
}
