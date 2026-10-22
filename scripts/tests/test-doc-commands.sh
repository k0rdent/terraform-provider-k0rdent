#!/usr/bin/env bash
#
# Proves scripts/check-doc-commands.sh fails on each kind of comment it exists
# to catch, and passes what a shell block may hold.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gate="$here/scripts/check-doc-commands.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "doc commands test: FAIL - $1" >&2; exit 1; }

# page <name> <fence tag> <line> writes a page with one block holding <line>.
page() {
  printf 'Text.\n\n```%s\n%s\n```\n' "$2" "$3" > "$work/$1.md"
}

refuses() {
  "$gate" "$work/$1.md" > /dev/null 2>&1 && fail "$1: passed: $2"
  true
}

passes() {
  "$gate" "$work/$1.md" > /dev/null 2>&1 || fail "$1: refused: $2"
}

page trailing sh 'cd ../cluster          # or ../cluster-own-project'
refuses trailing "a trailing comment"

page own-line bash '# Get a token'
refuses own-line "a comment on its own line"

page indented shell '    # indented'
refuses indented "an indented comment"

page clean sh 'cd ../cluster'
passes clean "a plain command"

page hash-inside sh 'echo "${#list[@]}" https://example.com/#anchor'
passes hash-inside "a # that isn't a comment"

page hcl hcl 'prefix = "jdoe" # yours'
passes hcl "a comment in a block that isn't shell"

printf 'Text.\n\n````md\n```sh\nls  # nested\n```\n````\n' > "$work/nested.md"
passes nested "a shell block shown inside another block"

echo 'doc commands test: PASS'
