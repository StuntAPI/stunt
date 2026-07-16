package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/stunt-adapters/stunt/internal/netutil"
)

// DoctorReport is a structured health check. BuildDoctor fills it in and
// PrintDoctor writes it as human-readable text.
type DoctorReport struct {
	Platform string // normalised platform identifier
	CAExists bool   // CA cert + key files present
	CADir    string // path to the CA directory (may not exist)
	CAError  string // non-empty if CA check failed (e.g. corrupt cert)
}

// BuildDoctor inspects the CA directory and platform, returning a read-only
// report. It never mutates state.
func BuildDoctor(caDir string) DoctorReport {
	r := DoctorReport{
		Platform: netutil.Platform(),
		CADir:    caDir,
	}
	certPath := filepath.Join(caDir, "ca.pem")
	keyPath := filepath.Join(caDir, "ca-key.pem")
	if !fileExistsCLI(certPath) || !fileExistsCLI(keyPath) {
		return r
	}
	r.CAExists = true
	// Try loading the CA to verify it is not corrupt.
	if _, err := netutil.EnsureCA(caDir); err != nil {
		r.CAExists = false
		r.CAError = err.Error()
	}
	return r
}

// PrintDoctor writes the report to out as human-readable text.
func PrintDoctor(out io.Writer, r DoctorReport) {
	fmt.Fprintf(out, "platform:   %s\n", r.Platform)
	status := "not found"
	if r.CAExists {
		status = "ok"
	}
	if r.CAError != "" {
		status = "error: " + r.CAError
	}
	fmt.Fprintf(out, "ca:         %s (%s)\n", r.CADir, status)
	fmt.Fprintf(out, "trust cmd:  run 'stunt trust' to install the CA\n")
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Print a health check (CA status, platform, etc.)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := cmd.Flags().GetString("manifest")
			r := BuildDoctor(caPath(manifestDir(path)))
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
