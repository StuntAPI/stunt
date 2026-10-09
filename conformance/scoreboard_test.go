package conformance

// TestMain lives in a _test.go file on purpose.
//
// `go test` only recognises TestMain in a file belonging to the test package
// (a *_test.go suffix). In a regular .go file it still compiles — the symbols
// are in the package, the build succeeds — but the generated test main never
// references it, so the linker dead-code-eliminates it and the function is
// silently never called. The suite reports "ok" either way.
//
// That is exactly what happened here: RUN_CONFORMANCE_SCOREBOARD has never
// produced a file in the history of this repo, including when set as its own
// docs describe, and nothing complained. Set the env var and the run stays
// green with no scoreboard on disk.
//
// So the scoreboard is runtime attestation that has never run. It is the only
// machine-readable answer to "did this check actually execute and pass?", which
// the matrix otherwise derives by parsing Record(...) call sites out of source
// text — a test that is skipped or renamed without being run still publishes
// its name.

import (
	"fmt"
	"os"
	"testing"
)

// ScoreboardPath, when set (RUN_CONFORMANCE_SCOREBOARD), receives the
// result dump after the whole run (TSV: sdk, adapter, check).
const ScoreboardPath = "RUN_CONFORMANCE_SCOREBOARD"

// TestMain dumps the scoreboard after a green run when the env var names
// a file — consumed by the case-study generator.
func TestMain(m *testing.M) {
	code := m.Run()
	if path := os.Getenv(ScoreboardPath); path != "" && code == 0 {
		f, err := os.Create(path)
		if err == nil {
			defer f.Close()
			for sdk, checks := range registered {
				for _, c := range checks {
					fmt.Fprintf(f, "%s\t%s\t%s\n", sdk, c.Adapter, c.Name)
				}
			}
		}
	}
	os.Exit(code)
}
