# stunt — local task runner.
#
# `just ci` is the canonical gate: the exact checks a PR must pass. A hosted CI
# job (GitHub Actions, etc.) can simply invoke `just ci` so there is one source
# of truth for "does this change ship".
#
# Recipes normalize GOROOT to empty below: on healthy machines `go` auto-detects
# its toolchain either way (empty == unset), and on a few dev machines a stray
# GOROOT points at a stale toolchain — emptying it makes `just ci` runnable
# everywhere. Harmless in CI.

# Unset any inherited GOROOT so `go` auto-detects its toolchain.
export GOROOT := ""

# Path to a freshly-built stunt binary used by adapter linting.
stunt_bin := "/tmp/stunt-ci"

# Compute the version string from git (tag, or commit hash).
version := `git describe --tags --always --dirty 2>/dev/null || echo dev`

# ldflags to inject the version into the binary.
ldflags := "-X github.com/stunt-adapters/stunt/internal/cli.Version=" + version

# default: show available recipes
default:
    @just --list

# ---- the canonical CI gate -------------------------------------------------
# Run every check a PR must pass. Exits non-zero on the first failure.
ci: build test vet fmt-check mod-tidy lint-adapters
    @echo "✓ all CI checks passed"

# ---- granular recipes ------------------------------------------------------

# Compile everything (including the CLI binary).
build:
    go build -ldflags "{{ldflags}}" ./...

# Run the full test suite under the race detector.
test:
    go test -race ./...

# `go vet` across all packages.
vet:
    go vet ./...

# Format all Go source in place.
fmt:
    gofmt -w .

# Fail if any Go source is not gofmt-clean (CI uses this; does not edit).
fmt-check:
    #!/usr/bin/env bash
    set -euo pipefail
    unformatted="$(gofmt -l .)"
    if [ -n "$unformatted" ]; then
        echo "gofmt would reformat:"
        echo "$unformatted"
        exit 1
    fi

# Fail if go.mod / go.sum are not tidy (catches a forgotten 'go mod tidy').
mod-tidy:
    #!/usr/bin/env bash
    set -euo pipefail
    cp go.mod go.mod.bak
    cp go.sum go.sum.bak
    trap 'rm -f go.mod.bak go.sum.bak' EXIT
    go mod tidy
    if ! diff -q go.mod go.mod.bak >/dev/null || ! diff -q go.sum go.sum.bak >/dev/null; then
        echo "go.mod/go.sum not tidy — run 'go mod tidy'"
        exit 1
    fi

# Build the stunt CLI and lint every shipped reference adapter; fail on any finding.
lint-adapters: build
    #!/usr/bin/env bash
    set -euo pipefail
    go build -ldflags "{{ldflags}}" -o {{stunt_bin}} ./cmd/stunt
    fail=0
    for a in adapters/*/; do
        printf ':: lint %s\n' "$a"
        {{stunt_bin}} adapter lint "./$a" || fail=1
    done
    exit "$fail"

# Lint a single adapter directory (usage: just lint-adapter ./adapters/echo-style).
lint-adapter dir:
    go build -ldflags "{{ldflags}}" -o {{stunt_bin}} ./cmd/stunt
    {{stunt_bin}} adapter lint {{dir}}

# Quick smoke test: build, init a temp manifest, up, curl, down. (host-safe.)
smoke:
    #!/usr/bin/env bash
    set -euo pipefail
    go build -ldflags "{{ldflags}}" -o {{stunt_bin}} ./cmd/stunt
    tmp="$(mktemp -d)"
    trap 'cd /; kill "$(cat "$tmp/up.pid" 2>/dev/null)" 2>/dev/null || true; rm -rf "$tmp"' EXIT
    cd "$tmp"
    {{stunt_bin}} init >/dev/null
    {{stunt_bin}} plan
    {{stunt_bin}} up &
    echo $! > up.pid
    sleep 1.5
    curl -s --max-time 3 http://127.0.0.1:8000/hello; echo
    {{stunt_bin}} down 2>/dev/null || kill "$(cat up.pid)" 2>/dev/null || true

# Clean the stunt binary build artifact.
clean:
    rm -f {{stunt_bin}}
