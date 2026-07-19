// Package adapters embeds the bundled reference adapters into the stunt
// binary so that commands like `stunt demo` work without requiring the
// adapter directory to be present on disk.
package adapters

import "embed"

// StripeStyleFS embeds the stripe-style reference adapter directory.
// At runtime the files are extracted to a temporary directory so the
// standard adapter loader (adapter.Load) can read them from disk.
//
//go:embed stripe-style
var StripeStyleFS embed.FS
