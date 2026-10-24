#!/usr/bin/env bash
#
# Proves scripts/check-public-text.sh fails on a planted hit, passes an
# allowlisted one, reads the index with --staged, exits 2 rather than 0 when
# it can't run, and that the pre-commit hook passes its exit code on. It runs
# against a scratch repository holding this repository's own deny-list and
# allowlist.
#
# Every planted hit is assembled at run time, so this file holds none itself.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gate="$here/scripts/check-public-text.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "public text test: FAIL - $1" >&2; exit 1; }

git -C "$work" init -q
mkdir -p "$work/.github/public-text" "$work/internal/provider"
cp "$here/.github/public-text/denylist.txt" "$here/.github/public-text/allowlist.txt" \
  "$here/.github/public-text/hashed.txt" "$work/.github/public-text/"

ticket="$(printf '%s-%s' ABC 123)"
editor="$(printf '%s%s' Cur sor)"

# expect CODE LABEL ARGS... runs the gate in the scratch repository and
# fails unless it exits CODE. Its output is kept in $work/out.
expect() {
  local want="$1" label="$2" rc=0
  shift 2
  (cd "$work" && "$gate" "$@") > "$work/out" 2>&1 || rc=$?
  [ "$rc" -eq "$want" ] || { cat "$work/out" >&2; fail "$label: exit $rc, want $want"; }
}

track() { git -C "$work" add -- "$@"; }

printf 'A plain line.\n' > "$work/clean.md"
track clean.md .github
expect 0 "a clean tree"

printf 'See %s for why.\n' "$ticket" > "$work/planted.md"
track planted.md
expect 1 "a planted ticket key"
grep -q "^planted.md:1:$ticket\$" "$work/out" || fail "the report doesn't name planted.md:1"
grep -q '^== ticket (1 hits)' "$work/out" || fail "the report doesn't name the ticket rule"

printf 'Hashed with SHA-256, mailed from noreply@example.com.\n' > "$work/planted.md"
track planted.md
expect 0 "allowlisted standards and addresses"

# An allowlist line is a path and a match together: the API's pagination
# field is allowed in its own file and nowhere else.
printf 'type Page struct { %s string }\n' "$editor" > "$work/internal/provider/cluster_events.go"
track internal/provider/cluster_events.go
expect 0 "an allowlisted match in its own file"
printf 'type Page struct { %s string }\n' "$editor" > "$work/internal/provider/other.go"
track internal/provider/other.go
expect 1 "the same match in another file"
git -C "$work" rm -q --cached internal/provider/other.go

# Platform detail: requirement ids, test stack names, measured run times and
# a test machine type. The machine type is allowed in the API's own spec only.
reqid="$(printf 'R%s' 12)"
stack="$(printf 'kind-%s' main)"
took="$(printf '1 hour %s minutes' 50)"
machine="$(printf 'GB%s' 300)"
measured="$(printf 'measured on %s-10-08' 2026)"
for planted in "Tracked as $reqid." "Run on $stack." "It took $took." "A $machine host." "As $measured."; do
  printf '%s\n' "$planted" > "$work/planted.md"
  track planted.md
  expect 1 "planted platform detail: $planted"
done
mkdir -p "$work/internal/k0rdent/spec"
printf -- '- %s\n' "$machine" > "$work/internal/k0rdent/spec/openapi.yaml"
printf 'Fixed.\n' > "$work/planted.md"
track planted.md internal/k0rdent/spec/openapi.yaml
expect 0 "the machine type in the API's spec"

# hashed.txt: a name as a word, a host under another subdomain, and a
# repository name with a suffix. The hit names the rule, never the hash.
person="$(printf 'Jus%s' tin)"
host="$(printf 'ci.pulse.k0%s.com' labs)"
repo="$(printf 'k0rdent-inter%s-2' nal)"
prefixed="$(printf 'reviewed-by-Jus%s' tin)"
inside="$(printf 'terraform-provider-k0rdent-inter%s' nal)"
for planted in "Ask $person." "See https://$host/x." "From $repo." "$prefixed." "Cloned $inside."; do
  printf '%s\n' "$planted" > "$work/planted.md"
  track planted.md
  expect 1 "a hashed name: $planted"
done
grep -q '^== private-repo (1 hits)' "$work/out" || fail "the report doesn't name the hashed rule"
printf 'Justice and a pulse.\n' > "$work/planted.md"
track planted.md
expect 0 "words that only contain a hashed name"
[ "$("$gate" --hash person - "$person")" = "$(grep -m1 '^person:-:' "$here/.github/public-text/hashed.txt")" ] \
  || fail "--hash doesn't print the line hashed.txt holds"

# Named files only.
printf 'See %s.\n' "$ticket" > "$work/planted.md"
track planted.md
expect 0 "a named clean file beside a dirty one" clean.md
expect 1 "a named dirty file" planted.md

# --staged reads the index, not the disk.
printf 'Fixed.\n' > "$work/planted.md"
expect 1 "--staged with the hit still staged" --staged
track planted.md
printf 'See %s.\n' "$ticket" > "$work/planted.md"
expect 0 "--staged with the hit only on disk" --staged
git -C "$work" checkout -q -- planted.md

# Can't run is never clean.
expect 2 "an untracked file named" not-tracked.md
cp "$work/.github/public-text/denylist.txt" "$work/deny.bak"
printf 'broken:-:(\n' >> "$work/.github/public-text/denylist.txt"
expect 2 "a deny-list rule that doesn't compile"
printf 'no flags here\n' > "$work/.github/public-text/denylist.txt"
expect 2 "a deny-list line without NAME:FLAGS:REGEX"
mv "$work/deny.bak" "$work/.github/public-text/denylist.txt"
cp "$work/.github/public-text/hashed.txt" "$work/hashed.bak"
printf 'person:-:not-a-hash\n' >> "$work/.github/public-text/hashed.txt"
expect 2 "a hashed.txt line without a SHA-256"
mv "$work/hashed.bak" "$work/.github/public-text/hashed.txt"
mv "$work/.github/public-text/allowlist.txt" "$work/allow.bak"
expect 2 "a missing allowlist"
mv "$work/allow.bak" "$work/.github/public-text/allowlist.txt"

# The pre-commit hook passes the gate's exit code on.
printf 'See %s.\n' "$ticket" > "$work/planted.md"
track planted.md
rc=0
(cd "$work" && "$here/scripts/hooks/pre-commit") > "$work/out" 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { cat "$work/out" >&2; fail "the pre-commit hook exited $rc on a staged hit, want 1"; }

echo 'public text test: PASS'
