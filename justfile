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
ldflags := "-X stuntapi.com/stunt/internal/cli.Version=" + version

# default: show available recipes
default:
    @just --list

# ---- the canonical CI gate -------------------------------------------------
# Run every check a PR must pass. Exits non-zero on the first failure.
ci: build test vet fmt-check mod-tidy cross-build lint-adapters
    @echo "✓ all CI checks passed"

# ---- release --------------------------------------------------------------
# Cut a release NOW from your machine — no GitHub Actions dependency.
# Works even when Actions can't run (billing exhausted, outage, fresh machine).
# Requires: a git tag at HEAD, and two tokens in the environment:
#   export TAP_GITHUB_TOKEN=<PAT: contents:write on stuntapi/homebrew-tap + stuntapi/winget>
#   export GITHUB_TOKEN="$(gh auth token)"
# Options:  --no-ci   skip the local CI gate (release even if ci cannot run).
release *args='':
    #!/usr/bin/env bash
    set -euo pipefail
    tag="$(git describe --exact-match --tags HEAD 2>/dev/null)" || {
        echo "⚠️  HEAD is not a tagged commit. Tag it first:" >&2
        echo "      git tag v0.x.y  &&  git push --tags" >&2
        exit 1
    }
    : "${TAP_GITHUB_TOKEN:?TAP_GITHUB_TOKEN required (fine-grained PAT: contents:write on stuntapi/homebrew-tap + stuntapi/winget)}"
    : "${GITHUB_TOKEN:?GITHUB_TOKEN required  (export GITHUB_TOKEN="\$(gh auth token)")}"
    command -v syft >/dev/null 2>&1 || {
        echo ">> syft not found — installing (GoReleaser's sboms pipe needs it)..."
        curl -sSfL https://raw.githubusercontent.com/anchore/syft/main/install.sh | sh -s -- -b /usr/local/bin
    }
    case "{{args}}" in *--no-ci*) echo ">> skipping local CI gate (--no-ci)";; *) just ci;; esac
    echo ">> cutting release $tag (local — no GitHub Actions)"
    goreleaser release --clean

# ---- granular recipes ------------------------------------------------------

# Compile everything (including the CLI binary).
build:
    go build -ldflags "{{ldflags}}" ./...

# Cross-compile every release target (linux/darwin/windows × amd64/arm64) to
# /dev/null. The host `build` only covers the host platform, so a Unix-only
# symbol (e.g. unix.Flock, syscall.Kill) compiles fine locally but breaks a
# GOOS=windows cross-compile — which is exactly what poisoned the v0.2.0 tag
# (caught only at release time, by GoReleaser). Mirrors the GoReleaser matrix
# (CGO_ENABLED=0, pure-Go) so what CI checks == what ships.
cross-build:
    #!/usr/bin/env bash
    set -euo pipefail
    fail=0
    for pair in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
        goos="${pair%/*}"; goarch="${pair#*/}"
        if CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -o /dev/null ./cmd/stunt; then
            echo "  ✓ $goos/$goarch"
        else
            echo "  ✗ $goos/$goarch"; fail=1
        fi
    done
    exit $fail

# Run the full test suite under the race detector.
test:
    go test -race ./...

# SDK conformance: run REAL provider SDKs (stripe-go, aws-sdk-go-v2,
# go-github) against booted adapters. Nested module under conformance/ so
# the SDK deps never touch the stunt binary's graph. Set
# RUN_CONFORMANCE_SCOREBOARD=<file> to dump a TSV of passed checks.
conformance:
    cd conformance && go test ./... -count=1 -race -v

# Node SDK conformance: stripe-node, octokit, twilio-node via bun,
# booting the REAL stunt binary (also end-to-end tests the CLI). Requires
# bun on PATH; builds the binary to /tmp first.
conformance-node:
    #!/bin/sh
    set -e
    command -v bun >/dev/null || { echo "bun not found on PATH"; exit 1; }
    # Build fresh — `just build` produces no binary, and /tmp/stunt-ci may
    # hold a stale artifact from an old lint run.
    go build -ldflags "{{ldflags}}" -o /tmp/stunt-ci ./cmd/stunt
    cd conformance/node && bun install --frozen-lockfile
    STUNT_BIN=/tmp/stunt-ci bun test

# Regenerate CONFORMANCE.md (the per-adapter SDK conformance matrix) from
# adapter manifests, conformance test sources and conformance/matrix.yaml.
# CI regenerates and fails on drift — rerun after touching adapters,
# Record(...) calls, node test sections, or matrix.yaml.
conformance-matrix:
    cd conformance && go run ./cmd/genmatrix

# Coverage-guided fuzzing — each target for the given time (default 30s;
# pass just fuzz 2m for longer rounds). The fuzz seed corpora also run as
# regular tests in `just test`, so discovered inputs stay pinned forever.
# Found failures are written to testdata/fuzz/<Target>/ — commit them.
fuzz t="30s":
    #!/bin/sh
    set -e
    for spec in \
        "internal/engine FuzzMatchRoute" \
        "internal/engine FuzzParseFormBody" \
        "internal/engine FuzzAdapterRequests" \
        "internal/adapter/runtime FuzzParseMultipart" \
        "internal/primitives/events FuzzValidateHeader"; do
        set -- $spec
        echo "== $1 $2"
        go test "$1" -run "^$2\$" -fuzz "^$2\$" -fuzztime={{t}} || {
            echo "FAIL: $2 — failing input in $1/testdata/fuzz/$2/"
            exit 1
        }
    done

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
