package cli

import (
	"fmt"
	"io"
)

// Version is overridden at link time via -ldflags "-X .../cli.Version=...".
var Version = "0.0.0-dev"

func runVersion(out io.Writer) error {
	_, err := fmt.Fprintf(out, "stunt %s\n", Version)
	return err
}
