#!/usr/bin/env bash
#
# Proves scripts/check-secrets.sh fails on a planted token, passes the fake's
# allowlisted credentials, reads the index with --staged, and that the
# pre-commit hook passes its exit code on. It runs against a scratch
# repository holding this repository's .gitleaks.toml. Needs the network the
# first time, to download the pinned gitleaks.
#
# Every planted token is assembled at run time, so this file holds none.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gate="$here/scripts/check-secrets.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
export TOOLS_BIN="${TOOLS_BIN:-$here/.tools/bin}"

fail() { echo "secrets test: FAIL - $1" >&2; exit 1; }

expect() {
  local want="$1" label="$2" rc=0
  shift 2
  (cd "$work" && "$@") > "$work/out" 2>&1 || rc=$?
  [ "$rc" -eq "$want" ] || { cat "$work/out" >&2; fail "$label: exit $rc, want $want"; }
}

git -C "$work" init -q
mkdir -p "$work/.github/public-text"
cp "$here/.gitleaks.toml" "$work/"
cp "$here/.github/public-text/denylist.txt" "$here/.github/public-text/allowlist.txt" \
  "$here/.github/public-text/hashed.txt" "$work/.github/public-text/"

# Fixed values, not random ones: gitleaks skips a value that holds one of its
# stopwords, such as "dead", and random hex held one in 3 of 800 runs.
# gitleaks 8.30.1 flags both. They're split so this file holds neither whole.
hex="$(printf '%s' b6cc8e3a c7f89b1a 0ba8a772 e333b53d c857f8b1 5d1df6c0 ab41b22a 4da72d39)"
token="$(printf '%s' 494ec599 b65d74d5 06e06de1 93b34ac5 ee6b)"

printf 'package x\n' > "$work/clean.go"
git -C "$work" add -A
expect 0 "a clean tree" "$gate"

printf 'token := "ghp_%s"\n' "$token" > "$work/planted.go"
git -C "$work" add planted.go
expect 1 "a planted GitHub token" "$gate"
expect 1 "a planted GitHub token, staged" "$gate" --staged

# The fake's prefix is allowed, and only the prefix: the same value behind
# another prefix is a finding, so the allowlist is what passes it.
printf 'client_secret = "k0r_fake_secret_%s"\n' "$hex" > "$work/planted.go"
git -C "$work" add planted.go
expect 0 "a k0r_fake_ credential" "$gate"
printf 'client_secret = "k0r_live_secret_%s"\n' "$hex" > "$work/planted.go"
git -C "$work" add planted.go
expect 1 "the same value without the fake's prefix" "$gate"

# --staged reads the index, not the disk.
printf 'package x\n' > "$work/planted.go"
expect 1 "--staged with the token still staged" "$gate" --staged
git -C "$work" add planted.go
printf 'token := "ghp_%s"\n' "$token" > "$work/planted.go"
expect 0 "--staged with the token only on disk" "$gate" --staged

# The pre-commit hook: the public-text check passes, then gitleaks fails it.
git -C "$work" add planted.go
expect 1 "the pre-commit hook on a staged token" "$here/scripts/hooks/pre-commit"

expect 2 "an unknown argument" "$gate" --bogus

echo 'secrets test: PASS'
