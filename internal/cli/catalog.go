package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/catalog"
)

// newCatalogCmd creates the "catalog" parent command for browsing the adapter
// registry.
func newCatalogCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Browse the adapter catalog",
		Long: `Browse the stunt adapter catalog — search for adapters and inspect their
details (git URL, pinned ref, tags).

The catalog is fetched from a remote JSON index and cached in-memory. If the
remote is unreachable, a small bundled fallback index is used so the command
always works offline.`,
	}
	cmd.PersistentFlags().String("catalog-url", "", "override the catalog index URL (default: $STUNT_CATALOG_URL or the stunt-project index)")
	cmd.AddCommand(newCatalogSearchCmd())
	cmd.AddCommand(newCatalogShowCmd())
	return cmd
}

func newCatalogSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search [query]",
		Short: "List adapters matching a name, description, or tag",
		Long: `Search the adapter catalog. The query is matched (case-insensitive) against
adapter names, descriptions, and tags. With no query, all known adapters are
listed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) > 0 {
				query = args[0]
			}
			flagURL, _ := cmd.Flags().GetString("catalog-url")
			url := resolveCatalogURL(flagURL)
			return runCatalogSearch(cmd.OutOrStdout(), url, query)
		},
	}
}

func newCatalogShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show full details for one adapter",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flagURL, _ := cmd.Flags().GetString("catalog-url")
			url := resolveCatalogURL(flagURL)
			return runCatalogShow(cmd.OutOrStdout(), url, args[0])
		},
	}
}

// resolveCatalogURL determines the index URL: flag override, then env, then
// the default constant.
func resolveCatalogURL(flagURL string) string {
	if flagURL != "" {
		return flagURL
	}
	if env := os.Getenv("STUNT_CATALOG_URL"); env != "" {
		return env
	}
	return catalog.DefaultIndexURL
}

// runCatalogSearch queries the catalog and prints matching entries (one line
// per entry: name, description, git URL).
func runCatalogSearch(out io.Writer, url, query string) error {
	idx := catalog.NewRemoteIndexWithClient(url, &http.Client{Timeout: 5 * time.Second}, catalog.DefaultCacheTTL)
	results, err := idx.Search(context.Background(), query)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintf(out, "no adapters found\n")
		return nil
	}
	for _, e := range results {
		fmt.Fprintf(out, "%-16s  %s\n", e.Name, e.Description)
		fmt.Fprintf(out, "                 %s\n", e.GitURL)
	}
	return nil
}

// runCatalogShow fetches a single entry and prints its full details,
// including a copy-pasteable manifest snippet and usage guidance.
func runCatalogShow(out io.Writer, url, name string) error {
	idx := catalog.NewRemoteIndexWithClient(url, &http.Client{Timeout: 5 * time.Second}, catalog.DefaultCacheTTL)
	e, err := idx.Get(context.Background(), name)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Name:        %s\n", e.Name)
	fmt.Fprintf(out, "Description: %s\n", e.Description)
	fmt.Fprintf(out, "Git URL:     %s\n", e.GitURL)
	fmt.Fprintf(out, "Latest ref:  %s\n", e.LatestRef)
	if len(e.Tags) > 0 {
		fmt.Fprintf(out, "Tags:        %s\n", strings.Join(e.Tags, ", "))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintf(out, "  stunt adapter add %s\n", e.Name)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  # or add manually to stunt.yaml:")
	fmt.Fprintf(out, "  services:\n    %s:\n", e.Name)
	if e.LatestRef != "" {
		fmt.Fprintf(out, "      adapter: %s@%s\n", e.GitURL, e.LatestRef)
	} else {
		fmt.Fprintf(out, "      adapter: %s\n", e.GitURL)
	}
	return nil
}
