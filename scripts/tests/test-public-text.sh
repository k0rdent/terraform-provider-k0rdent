#!/usr/bin/env bash
#
# Proves scripts/check-public-text.sh fails on a planted hit, passes an
# allowlisted one, reads the index with --staged, exits 2 rather than 0 when
# it can't run, and that the pre-commit hook passes its exit code on. It runs
# against a scratch repository holding this repository's own deny-list and
# allowlist, and a hashed.txt of made-up names.
#
# Every planted hit is assembled at run time, so this file holds none itself.
set -euo pipefail
# The user's own git config, such as hooks or a comment character, stays out.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gate="$here/scripts/check-public-text.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "public text test: FAIL - $1" >&2; exit 1; }

git -C "$work" init -q
mkdir -p "$work/.github/public-text" "$work/internal/provider"
cp "$here/.github/public-text/denylist.txt" "$here/.github/public-text/allowlist.txt" \
  "$work/.github/public-text/"
{
  "$gate" --hash person - Zorvath
  "$gate" --hash host i quillon.example.net
  "$gate" --hash repo - acme-vault
  "$gate" --hash channel i '#quill-'
  "$gate" --hash stack i blue-rig
} > "$work/.github/public-text/hashed.txt"

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

# Platform detail: requirement ids, measured run times and a test machine
# type. The machine type is allowed in the API's own spec only.
reqid="$(printf 'R%s' 12)"
took="$(printf '1 hour %s minutes' 50)"
machine="$(printf 'GB%s' 300)"
measured="$(printf 'measured on %s-10-08' 2026)"
for planted in "Tracked as $reqid." "It took $took." "A $machine host." "As $measured."; do
  printf '%s\n' "$planted" > "$work/planted.md"
  track planted.md
  expect 1 "planted platform detail: $planted"
done
mkdir -p "$work/internal/k0rdent/spec"
printf -- '- %s\n' "$machine" > "$work/internal/k0rdent/spec/openapi.yaml"
printf 'Fixed.\n' > "$work/planted.md"
track planted.md internal/k0rdent/spec/openapi.yaml
expect 0 "the machine type in the API's spec"

# hashed.txt: a name as a word, a host under another subdomain, a repository
# name with a suffix, a name after a prefix, a "#name-" prefix, a name inside
# a longer word, and a name in another case. The hit names the rule, never
# the hash.
for planted in "Ask Zorvath." "See https://ci.quillon.example.net/x." "From acme-vault-2." \
  "reviewed-by-Zorvath." "In #quill-ops." "Cloned my-acme-vault-fork." "Run on BLUE-RIG."; do
  printf '%s\n' "$planted" > "$work/planted.md"
  track planted.md
  expect 1 "a hashed name: $planted"
done
grep -q '^== stack (1 hits)' "$work/out" || fail "the report doesn't name the hashed rule"
printf 'Zorvathian, acmevault and quillon.example.org.\n' > "$work/planted.md"
track planted.md
expect 0 "words that only contain a hashed name"
sha256() { if command -v sha256sum > /dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }
[ "$("$gate" --hash person - Zorvath)" = "person:-:$(printf Zorvath | sha256)" ] \
  || fail "--hash doesn't print NAME:-:SHA256 of the word"
[ "$("$gate" --hash host i QUILLON.example.net)" = "host:i:$(printf quillon.example.net | sha256)" ] \
  || fail "--hash with i doesn't hash the word lowercased"

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

# Pull request text, which CI scans beside the files. The report names the
# source and line.
printf 'Fix the plan\n' > "$work/title"
expect 0 "a clean title" --text title "$work/title"
printf 'Ask Zorvath\n' > "$work/title"
expect 1 "a hashed name in a title" --text title "$work/title"
grep -q '^title:1:Zorvath$' "$work/out" || fail "the report doesn't name title:1"
printf 'One.\n\nSee %s.\n' "$ticket" > "$work/body"
expect 1 "a ticket key in a body" --text body "$work/body"
grep -q "^body:3:$ticket\$" "$work/out" || fail "the report doesn't name body:3"
GITHUB_ACTIONS=true expect 1 "a hit annotated in CI" --text body "$work/body"
grep -q "^::error title=public text::rule ticket matched '$ticket' in body line 3\$" "$work/out" \
  || fail "the annotation doesn't name the source"
: > "$work/body"
expect 0 "an empty body" --text body "$work/body"
printf 'Hashed with SHA-256.\n' > "$work/body"
expect 0 "allowlisted text in a body" --text body "$work/body"
# A hit's text can't become a workflow command, or color the log.
account="$(printf '1234567890%s' 12)"
printf 'aws ##[add-mask] %s\naws \033[m %s\naws cost %% of %s\n' "$account" "$account" "$account" > "$work/body"
GITHUB_ACTIONS=true expect 1 "a hit that looks like a workflow command" --text body "$work/body"
if grep -q '##\[' "$work/out" || grep -q "$(printf '\033')" "$work/out"; then
  cat "$work/out" >&2
  fail "the report prints a hit's ##[ or escape character as it is"
fi
grep -q '^::error title=public text::.*cost %25 of' "$work/out" || fail "the annotation doesn't escape %"
printf 'Fix the plan\n' > "$work/title"
rc=0
(cd "$work/internal" && "$gate" --text title ../title) > "$work/out" 2>&1 || rc=$?
[ "$rc" -eq 0 ] || { cat "$work/out" >&2; fail "--text with a path relative to a subdirectory: exit $rc, want 0"; }

# Commit messages over a range.
g() { git -C "$work" -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false "$@"; }
g commit -q -m "Add the clean files"
base="$(g rev-parse HEAD)"
g commit -q --allow-empty -m "Fix the plan" -m "Reviewed by Zorvath."
hit="$(g rev-parse --short=12 HEAD)"
g commit -q --allow-empty -m "Fix the docs"
expect 1 "a hashed name in a commit message" --commits "$base..HEAD"
grep -q "^commit $hit:3:Zorvath\$" "$work/out" || fail "the report doesn't name the commit and line"
expect 0 "a clean commit message" --commits "HEAD~1..HEAD"
printf 'Ask Zorvath\n' > "$work/title"
expect 1 "commits and text together" --commits "HEAD~1..HEAD" --text title "$work/title" --text body "$work/clean.md" --commits "HEAD~1..HEAD"
grep -q '^title:1:Zorvath$' "$work/out" || fail "--text after --commits isn't scanned"
# Dependabot signs its commits off with this address, and Go advisory ids
# look like ticket keys.
g commit -q --allow-empty -m "Bump a module" -m "Fixes GO-2026-1234." \
  -m "Signed-off-by: dependabot[bot] <$(printf 'support@%s' github.com)>"
expect 0 "Dependabot's sign-off and a Go advisory id" --commits "HEAD~1..HEAD"
g log -1 --format=%B > "$work/body"
expect 1 "Dependabot's address outside a commit" --text body "$work/body"

# The workflow's own step, run as written, on a merge commit like CI's
# checkout. Text run as a command would print the machine type, a hit.
mkdir -p "$work/scripts" "$work/runner"
cp "$gate" "$work/scripts/"
awk '/^        run: \|$/ { on = 1; next } on && /^          / { sub(/^          /, ""); print; next } on { exit }' \
  "$here/.github/workflows/pr-text.yml" > "$work/step.sh"
[ -s "$work/step.sh" ] || fail "no run block in pr-text.yml"
pr() { # BRANCH MESSAGE merges a branch holding one commit
  g checkout -q -b "$1"
  g commit -q --allow-empty -m "$2"
  g checkout -q -
  g merge -q --no-ff --no-edit "$1"
}
step() { # WANT LABEL TITLE BODY, where a BODY of null is no description
  local rc=0
  python3 -c 'import json, sys; print(json.dumps({"pull_request": {"title": sys.argv[1], "body": None if sys.argv[2] == "null" else sys.argv[2]}}))' \
    "$3" "$4" > "$work/event.json"
  (cd "$work" && GITHUB_EVENT_PATH="$work/event.json" RUNNER_TEMP="$work/runner" bash -e step.sh) > "$work/out" 2>&1 || rc=$?
  [ "$rc" -eq "$1" ] || { cat "$work/out" >&2; fail "the workflow step, $2: exit $rc, want $1"; }
}
pr clean-pr "Fix the tests"
step 0 "a clean pull request after a hit on its base" "Fix the tests" null
# shellcheck disable=SC2016
shell='"a" '"'b'"'; `printf GB%s 300` $(printf GB%s 300) $(touch pwned; echo injected)'
step 0 "quotes, a semicolon, a backtick and a command substitution" "$shell" "$shell"
[ ! -e "$work/pwned" ] || fail "the workflow step ran the title as a command"
step 1 "a hashed name in the title" "Ask Zorvath" ""
grep -q '^title:1:Zorvath$' "$work/out" || fail "the workflow step doesn't name the title"
step 1 "a hashed name in the description" "Fix the tests" "One.
Ask Zorvath."
grep -q '^body:2:Zorvath$' "$work/out" || fail "the workflow step doesn't name the description"
pr named-pr "Thanks Zorvath"
step 1 "a hashed name in a commit message" "Fix the tests" null
grep -q '^commit [0-9a-f]\{12\}:1:Zorvath$' "$work/out" || fail "the workflow step doesn't name the commit"

# Can't run is never clean.
expect 2 "a range git can't read" --commits "nothing..HEAD"
expect 2 "--text without a file" --text title
expect 2 "--text naming a missing file" --text title "$work/missing"
expect 2 "--commits beside --staged" --commits "HEAD~1..HEAD" --staged
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

# The commit-msg hook refuses a hit in the message, but not one in git's
# comments or in the diff `commit -v` shows below them.
mkdir "$work/hooks"
printf '#!/bin/sh\nexec "%s/scripts/hooks/commit-msg" "$@"\n' "$here" > "$work/hooks/commit-msg"
chmod +x "$work/hooks/commit-msg"
printf 'Ask Zorvath.\n' > "$work/named.md"
g add named.md
g -c core.hooksPath="$work/hooks" commit -q -m "Add a file" > "$work/out" 2>&1 \
  || { cat "$work/out" >&2; fail "the commit-msg hook refused a clean message"; }
rc=0
g -c core.hooksPath="$work/hooks" commit -q --allow-empty -m "Fix the plan" -m "Ask Zorvath." > "$work/out" 2>&1 || rc=$?
[ "$rc" -ne 0 ] || fail "the commit-msg hook let a hashed name in a message through"
grep -q '^commit message:3:Zorvath$' "$work/out" || { cat "$work/out" >&2; fail "the commit-msg hook doesn't name the line"; }
printf 'Fixed.\n' > "$work/named.md"
g add named.md
rc=0
GIT_EDITOR="sed -i.bak -e '1s/^/Fix the plan/' -e '2s/^/# Zorvath /' -e '\$s/\$/ Zorvath/'" \
  g -c core.hooksPath="$work/hooks" commit -q -v > "$work/out" 2>&1 || rc=$?
[ "$rc" -eq 0 ] || { cat "$work/out" >&2; fail "the commit-msg hook read git's comments or the diff: exit $rc, want 0"; }

# The pre-commit hook passes the gate's exit code on.
printf 'See %s.\n' "$ticket" > "$work/planted.md"
track planted.md
rc=0
(cd "$work" && "$here/scripts/hooks/pre-commit") > "$work/out" 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { cat "$work/out" >&2; fail "the pre-commit hook exited $rc on a staged hit, want 1"; }

echo 'public text test: PASS'
