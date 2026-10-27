#!/usr/bin/env bash
#
# Runs goreleaser for make release and make release-snapshot, with both tools
# fetched fresh into a new directory for this run:
#
#   - syft, at its pinned checksum, by install-syft.sh
#   - goreleaser, at GORELEASER_VERSION, through the Go module proxy, which
#     checks it against the checksum database
#
# Nothing already in .tools/bin or on PATH runs, so a binary committed there
# can't run beside the signing key. The directory goes when this exits, an
# interrupted run included.
#
# Usage: GORELEASER_VERSION=v2.18.2 scripts/run-goreleaser.sh release --clean
set -euo pipefail

cd "$(dirname "$0")/.."
: "${GORELEASER_VERSION:?set GORELEASER_VERSION, as the Makefile does}"

# Tests point this at a stand-in, so they don't download syft.
tools="$("${SYFT_INSTALLER:-./scripts/install-syft.sh}")"
trap 'rm -rf "$tools"' EXIT

GOBIN="$tools" go install "github.com/goreleaser/goreleaser/v2@$GORELEASER_VERSION"
PATH="$tools:$PATH" "$tools/goreleaser" "$@"
