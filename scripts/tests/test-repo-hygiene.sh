#!/usr/bin/env bash
#
# Proves each hygiene gate fires, by driving each one against a repository
# built to trip it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gate="$here/scripts/repo-hygiene.sh"
ignored_gate="$here/scripts/check-tracked-ignored.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "repo hygiene test: FAIL - $1" >&2; exit 1; }

# A throwaway repository with one committed file, and the gate copied in so it
# runs against that tree rather than this one.
setup() {
  rm -rf "$work/repo" && mkdir -p "$work/repo/scripts"
  cd "$work/repo"
  git init -q .
  git config user.email t@example.invalid
  git config user.name test
  git config commit.gpgsign false
  cp "$gate" scripts/repo-hygiene.sh
  cp "$ignored_gate" scripts/check-tracked-ignored.sh
  echo ok > kept.txt
}

commit() { git add -A && git commit -qm t; }

expect_fail_script() {
  local script="$1" what="$2" needle="$3"
  if "./scripts/$script" >"$work/out" 2>&1; then
    cat "$work/out" >&2
    fail "$what"
  fi
  grep -Fq "$needle" "$work/out" || { cat "$work/out" >&2; fail "$what: message did not mention $needle"; }
}

expect_fail() {
  if ./scripts/repo-hygiene.sh >"$work/out" 2>&1; then
    cat "$work/out" >&2
    fail "$1"
  fi
  grep -Fq "$2" "$work/out" || { cat "$work/out" >&2; fail "$1: message did not mention $2"; }
}

# 1. A tracked path that an ignore rule covers. repo-hygiene.sh does not assert
#    this: scripts/check-tracked-ignored.sh does.
setup
echo cached > thing.log        # *.log is ignored by this repo's own rules
echo '*.log' > .gitignore
git add -A && git add -f thing.log && git commit -qm t
expect_fail_script check-tracked-ignored.sh \
  "an ignored-but-tracked file was accepted" "match an ignore rule"

# 1b. And it passes on a tree with nothing forced past a rule, so it is not
#     simply always red.
setup
echo '*.log' > .gitignore
commit
./scripts/check-tracked-ignored.sh >/dev/null 2>&1 ||
  fail "a tree with no tracked ignored file was rejected"

# 2. Too many files: many small files rather than one big one, which a size cap
#    alone would not catch.
setup
mkdir -p cache
for i in $(seq 1 60); do echo x > "cache/f$i"; done
commit
MAX_FILES=20 expect_fail "a file-count explosion was accepted" "tracked files exceeds"

# 3. Total size, with every file small.
setup
mkdir -p cache
for i in $(seq 1 10); do head -c 20000 /dev/zero | tr '\0' 'x' > "cache/f$i"; done
commit
MAX_TOTAL_BYTES=50000 expect_fail "an oversized tree was accepted" "tracked content is"

# 4. And a clean tree passes, so the gate is not simply always red.
setup
commit
./scripts/repo-hygiene.sh >/dev/null 2>&1 || fail "a clean tree was rejected"

echo "repo hygiene test: PASS"
