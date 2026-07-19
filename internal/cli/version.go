package cli

import (
	"fmt"
	"io"
)

// Version is overridden at link time via -ldflags "-X .../cli.Version=...".
// When built via `just build` or `just ci`, it is set to the git describe
// output (tag or commit hash). When installed via `go install` without
// ldflags, it defaults to "0.0.0-dev", meaning an untagged/install build.
var Version = "0.0.0-dev"

func runVersion(out io.Writer) error {
	_, err := fmt.Fprintf(out, "stunt %s\n", Version)
	return err
}
