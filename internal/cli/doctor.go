package cli

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"stuntapi.com/stunt/internal/manifest"
	"stuntapi.com/stunt/internal/netutil"
)

// DoctorReport is a structured health check. BuildDoctor fills it in and
// PrintDoctor writes it as human-readable text.
type DoctorReport struct {
	Platform string // normalised platform identifier
	CAExists bool   // CA cert + key files present
	CADir    string // path to the CA directory (may not exist)
	CAError  string // non-empty if CA check failed (e.g. corrupt cert)

	// Manifest checks.
	ManifestPath  string          // path to stunt.yaml (may not exist)
	ManifestFound bool            // true if stunt.yaml exists
	ManifestError string          // non-empty if parse/validate failed
	ServiceCount  int             // number of services (0 if not loaded)
	UnknownFields []string        // unknown top-level keys (typos)
	ServiceChecks []DoctorService // per-service adapter/port checks
}

// DoctorService is the per-service health entry.
type DoctorService struct {
	Name         string
	AdapterSpec  string // adapter source spec (empty for rules-only)
	AdapterOK    bool   // adapter loaded successfully
	AdapterError string // non-empty if adapter failed to load
	BasePort     int    // assigned port (port mode, 0 otherwise)
	PortInUse    bool   // true if the port is currently bound by something
}

// BuildDoctor inspects the platform, CA, manifest, adapters, and ports,
// returning a read-only report. It never mutates state (except the port
// probe which performs a harmless TCP connect to 127.0.0.1).
func BuildDoctor(caDir, manifestPath string) DoctorReport {
	r := DoctorReport{
		Platform:     netutil.Platform(),
		CADir:        caDir,
		ManifestPath: manifestPath,
	}

	// --- CA check ---
	certPath := filepath.Join(caDir, "ca.pem")
	keyPath := filepath.Join(caDir, "ca-key.pem")
	if fileExistsCLI(certPath) && fileExistsCLI(keyPath) {
		r.CAExists = true
		if _, err := netutil.EnsureCA(caDir); err != nil {
			r.CAExists = false
			r.CAError = err.Error()
		}
	}

	// --- Manifest check ---
	if fileExistsCLI(manifestPath) {
		r.ManifestFound = true
		m, err := manifest.Load(manifestPath)
		if err != nil {
			r.ManifestError = err.Error()
		} else if err := manifest.Validate(m); err != nil {
			r.ManifestError = err.Error()
		} else {
			r.ServiceCount = len(m.Services)
			r.UnknownFields = m.UnknownFields
			m.Network.Defaults()
			r.ServiceChecks = checkDoctorServices(m, filepath.Dir(manifestPath))
		}
	}

	return r
}

// checkDoctorServices checks each service's adapter loadability and (for
// port mode) whether the assigned port is currently in use.
func checkDoctorServices(m *manifest.Manifest, manifestDir string) []DoctorService {
	names := sortedServiceNames(m.Services)
	out := make([]DoctorService, 0, len(names))
	port := m.Network.BasePort
	for _, name := range names {
		svc := m.Services[name]
		ds := DoctorService{Name: name, AdapterSpec: svc.Adapter, BasePort: port}
		if svc.Adapter != "" {
			a, err := planResolveAdapter(svc.Adapter, manifestDir)
			if err != nil {
				ds.AdapterError = err.Error()
			} else {
				ds.AdapterOK = true
				_ = a // adapter loaded successfully; details shown by plan
			}
		}
		if m.Network.Mode == "port" && port > 0 {
			ds.PortInUse = portInUse(port)
		}
		out = append(out, ds)
		port++
	}
	return out
}

// portInUse reports whether something is currently listening on the given
// TCP port on 127.0.0.1. This is a harmless connect attempt (host-safe).
func portInUse(port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false // nothing listening
	}
	conn.Close()
	return true
}

// PrintDoctor writes the report to out as human-readable text.
func PrintDoctor(out io.Writer, r DoctorReport) {
	fmt.Fprintf(out, "platform:   %s\n", r.Platform)

	// --- CA ---
	caStatus := "not found"
	if r.CAExists {
		caStatus = "ok"
	}
	if r.CAError != "" {
		caStatus = "error: " + r.CAError
	}
	fmt.Fprintf(out, "ca:         %s (%s)\n", r.CADir, caStatus)
	fmt.Fprintf(out, "trust cmd:  run 'stunt trust' to install the CA\n")

	// --- Manifest ---
	fmt.Fprintln(out)
	fmt.Fprintln(out, "manifest:")
	if !r.ManifestFound {
		fmt.Fprintf(out, "  %s  not found\n", r.ManifestPath)
	} else if r.ManifestError != "" {
		fmt.Fprintf(out, "  %s  parse error: %s\n", r.ManifestPath, r.ManifestError)
	} else {
		fmt.Fprintf(out, "  %s  ok (%d service(s))\n", r.ManifestPath, r.ServiceCount)
	}
	for _, f := range r.UnknownFields {
		fmt.Fprintf(out, "  WARNING: unknown field %q (may be a typo)\n", f)
	}

	// --- Services ---
	if len(r.ServiceChecks) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "services:")
		for _, ds := range r.ServiceChecks {
			// Adapter status.
			if ds.AdapterSpec != "" {
				if ds.AdapterOK {
					fmt.Fprintf(out, "  %s  adapter %s  ok\n", ds.Name, ds.AdapterSpec)
				} else {
					fmt.Fprintf(out, "  %s  adapter %s  ERROR: %s\n", ds.Name, ds.AdapterSpec, ds.AdapterError)
				}
			} else {
				fmt.Fprintf(out, "  %s  (rules-only)\n", ds.Name)
			}
			// Port status.
			if ds.BasePort > 0 {
				if ds.PortInUse {
					fmt.Fprintf(out, "    port %d  IN USE (is stunt already running?)\n", ds.BasePort)
				} else {
					fmt.Fprintf(out, "    port %d  free\n", ds.BasePort)
				}
			}
		}
	}
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Print a health check (CA status, manifest, adapters, ports)",
		Long: `Print a diagnostic health check covering everything stunt needs to run:

  - Platform and OS
  - CA status (generated? installed in the trust store?)
  - Manifest validity and service count
  - Per-service adapter load status and port availability

Run this when something is not working — it surfaces the most common failure
modes (missing CA, unloadable adapter, port conflict) in one place.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			r := BuildDoctor(caPath(manifestDir(path)), path)
			PrintDoctor(cmd.OutOrStdout(), r)
			return nil
		},
	}
}

// fileExistsCLI checks if a file exists. Local to the CLI package to avoid
// importing os in every file.
func fileExistsCLI(path string) bool {
	if st, err := osStat(path); err == nil && !st.IsDir() {
		return true
	}
	return false
}
