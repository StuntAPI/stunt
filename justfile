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
ci: build test vet fmt-check mod-tidy lint-adapters
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
