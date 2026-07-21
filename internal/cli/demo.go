package cli

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/adapters"
	"stuntapi.com/stunt/internal/engine"
	"stuntapi.com/stunt/internal/manifest"
)

// newDemoCmd creates the "demo" command — a zero-config way to experience
// stunt's stateful API simulation. It boots the bundled stripe-style
// adapter, prints copy-pasteable curl commands, and runs a local webhook
// sink so users can see webhooks fire in real time.
func newDemoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Boot a stateful Stripe-style sim with copy-pasteable curl commands",
		Long: `Start a stateful Stripe-style API simulator (charges, customers, balance,
webhooks) with zero configuration.

This command boots the bundled stripe-style reference adapter on a free port,
starts a local webhook sink so you can see webhook events fire, and prints a
numbered list of copy-pasteable curl commands that demonstrate stunt's
stateful magic: create a charge, list it, capture it, refund it, and watch
the webhook events arrive.

All data is synthetic. This does not call any real API.

Press Ctrl-C to stop.`,
		RunE: runDemo,
	}
	cmd.Flags().Int("port", 0, "listen port (0 = OS-assigned high port)")
	cmd.Flags().Bool("no-webhook-sink", false, "skip the local webhook sink")
	return cmd
}

func runDemo(cmd *cobra.Command, args []string) error {
	port, _ := cmd.Flags().GetInt("port")
	noWebhookSink, _ := cmd.Flags().GetBool("no-webhook-sink")
	out := cmd.OutOrStdout()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runDemoServe(ctx, out, port, noWebhookSink)
}

// runDemoServe is the testable core of the demo command. It resolves the
// adapter, starts the engine, prints the curl menu, and blocks until ctx
// is canceled.
func runDemoServe(ctx context.Context, out io.Writer, port int, noWebhookSink bool) error {
	// 1. Extract the embedded adapter to a temp directory.
	adapterDir, err := extractEmbeddedAdapter(adapters.StripeStyleFS, "stripe-style")
	if err != nil {
		return fmt.Errorf("demo: extract adapter: %w", err)
	}
	defer os.RemoveAll(adapterDir)

	// 2. Create a temp manifest pointing at the adapter.
	tmpDir, err := os.MkdirTemp("", "stunt-demo-*")
	if err != nil {
		return fmt.Errorf("demo: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// 3. Optionally start the webhook sink.
	var sink *webhookSink
	webhookURL := ""
	if !noWebhookSink {
		sink = newWebhookSink(out)
		sinkURL, err := sink.start(ctx)
		if err != nil {
			return fmt.Errorf("demo: start webhook sink: %w", err)
		}
		webhookURL = sinkURL
	}

	// 4. Build the manifest.
	manifestPath := filepath.Join(tmpDir, "stunt.yaml")
	svc := manifest.Service{Adapter: adapterDir}
	if webhookURL != "" {
		svc.Config = map[string]any{"webhook_url": webhookURL}
	}
	m := &manifest.Manifest{
		Path:    manifestPath,
		Version: 1,
		Network: manifest.Network{Mode: "port", BasePort: port},
		Services: map[string]manifest.Service{
			"stripe": svc,
		},
	}
	if err := manifest.Save(m, manifestPath); err != nil {
		return fmt.Errorf("demo: write manifest: %w", err)
	}

	// 5. Create and start the engine.
	e, err := engine.New(m)
	if err != nil {
		return fmt.Errorf("demo: create engine: %w", err)
	}
	defer e.Close()

	addrs, cancelServe, err := e.ServeForTest(ctx)
	if err != nil {
		return fmt.Errorf("demo: start engine: %w", err)
	}
	defer cancelServe()

	baseURL := addrs["stripe"]

	// 6. Print the demo output.
	printDemoHeader(out, baseURL, webhookURL)
	printDemoCurlMenu(out, baseURL)

	// 7. Block until Ctrl-C.
	fmt.Fprintln(out)
	fmt.Fprintln(out, "stunt demo — Ctrl-C to stop")
	<-ctx.Done()
	fmt.Fprintln(out, "stopped.")
	return nil
}

// printDemoHeader prints the introduction explaining what the user is seeing.
func printDemoHeader(out io.Writer, baseURL, webhookURL string) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "┌──────────────────────────────────────────────────────────────────┐")
	fmt.Fprintln(out, "│                    stunt demo — Stripe-style sim                 │")
	fmt.Fprintln(out, "├──────────────────────────────────────────────────────────────────┤")
	fmt.Fprintln(out, "│  A fully stateful payments API simulator running locally.        │")
	fmt.Fprintln(out, "│  All data is synthetic — no real API is called.                  │")
	fmt.Fprintln(out, "│                                                                  │")
	fmt.Fprintln(out, "│  Stateful: charges you create persist and show up in lists.      │")
	fmt.Fprintln(out, "│  Webhooks: events fire to a local sink in real time.             │")
	fmt.Fprintln(out, "│  Auth: use sk_test_demo (dev bypass — no token minting needed).  │")
	fmt.Fprintln(out, "└──────────────────────────────────────────────────────────────────┘")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  API URL:       %s\n", baseURL)
	if webhookURL != "" {
		fmt.Fprintf(out, "  Webhook sink:  %s\n", webhookURL)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Copy-paste these curl commands (in another terminal):")
	fmt.Fprintln(out)
}

// printDemoCurlMenu prints the numbered list of copy-pasteable curl commands.
func printDemoCurlMenu(out io.Writer, baseURL string) {
	auth := `-H "Authorization: Bearer sk_test_demo"`

	// 1. Create a charge
	fmt.Fprintln(out, "  1. Create a charge (creates state + fires a webhook):")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "     curl -s %s/v1/charges \\\n", baseURL)
	fmt.Fprintf(out, "       -X POST %s \\\n", auth)
	fmt.Fprintln(out, `       -H "Content-Type: application/json" \`)
	fmt.Fprintln(out, `       -d '{"amount": 4200, "currency": "usd"}'`)
	fmt.Fprintln(out)

	// 2. List charges
	fmt.Fprintln(out, "  2. List charges (shows the one you just created — stateful!):")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "     curl -s %s %s/v1/charges\n", auth, baseURL)
	fmt.Fprintln(out)

	// 3. Capture
	fmt.Fprintln(out, "  3. Capture the charge (transitions pending → succeeded):")
	fmt.Fprintln(out, "     # Replace ch_1 with the id from step 1:")
	fmt.Fprintf(out, "     curl -s %s/v1/charges/ch_1/capture \\\n", baseURL)
	fmt.Fprintf(out, "       -X POST %s\n", auth)
	fmt.Fprintln(out)

	// 4. Refund
	fmt.Fprintln(out, "  4. Refund the charge (transitions succeeded → refunded):")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "     curl -s %s/v1/charges/ch_1/refund \\\n", baseURL)
	fmt.Fprintf(out, "       -X POST %s\n", auth)
	fmt.Fprintln(out)

	// 5. Balance
	fmt.Fprintln(out, "  5. Check the balance:")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "     curl -s %s %s/v1/balance\n", auth, baseURL)
	fmt.Fprintln(out)

	// Webhook note
	fmt.Fprintln(out, "  Webhook events appear above with a [webhook] prefix as you")
	fmt.Fprintln(out, "  create, capture, and refund charges.")
}

// --- embedded adapter extraction ---

// extractEmbeddedAdapter writes an embedded adapter directory (from the
// go:embed FS) to a temporary directory on disk and returns its path. The
// caller must remove the directory when done (e.g. via defer).
func extractEmbeddedAdapter(fsys embed.FS, root string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "stunt-adapter-*")
	if err != nil {
		return "", err
	}
	target := filepath.Join(tmpDir, root)
	if err := copyEmbedDir(fsys, root, target); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}
	return target, nil
}

// copyEmbedDir recursively copies all files under srcRoot in the embed.FS
// to dstDir on disk.
func copyEmbedDir(fsys embed.FS, srcRoot, dstDir string) error {
	return fs.WalkDir(fsys, srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fsys.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", target, err)
		}
		return nil
	})
}

// --- webhook sink ---

// webhookSink is a tiny HTTP server that receives webhook POSTs and prints
// each event to the provided writer with a clear [webhook] prefix.
type webhookSink struct {
	server *http.Server
	out    io.Writer
	mu     sync.Mutex
	events []map[string]any
}

func newWebhookSink(out io.Writer) *webhookSink {
	return &webhookSink{out: out}
}

// start begins listening on a free port and returns the sink's URL.
func (s *webhookSink) start(ctx context.Context) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go s.server.Serve(ln)

	url := "http://" + ln.Addr().String()

	// Shut down when ctx is canceled.
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutCtx)
	}()

	return url, nil
}

func (s *webhookSink) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	defer r.Body.Close()

	var env map[string]any
	eventType := "unknown"
	if err := json.Unmarshal(body, &env); err == nil {
		if t, ok := env["type"].(string); ok {
			eventType = t
		}
		s.mu.Lock()
		s.events = append(s.events, env)
		s.mu.Unlock()
	}

	fmt.Fprintf(s.out, "  [webhook] %s\n", eventType)
	w.WriteHeader(http.StatusOK)
}
