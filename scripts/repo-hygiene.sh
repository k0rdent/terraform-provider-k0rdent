#!/usr/bin/env bash
#
# Fails when the tracked tree grows far past what this repository holds. A build
# cache added by mistake is tens of thousands of small files, which no per-file
# size cap catches, so this counts files and total bytes. Large single files,
# archives and build output are refused at push by the repository ruleset, and
# scripts/check-tracked-ignored.sh owns the .gitignore rule.
#
# Needs no credential and no sibling checkout.
set -euo pipefail

# Raise these in a commit that says what earned the files, never to make a red
# run green.
MAX_FILES="${MAX_FILES:-1000}"
MAX_TOTAL_BYTES="${MAX_TOTAL_BYTES:-26214400}" # 25 MiB

fail() {
  echo "::error title=Repository hygiene::$1"
  echo "repo hygiene: FAIL - $1" >&2
  exit 1
}

# Sizes come from the object store rather than from disk, so this measures what
# is committed and not what happens to be lying in the working tree.
listing="$(git ls-tree -r -l --full-tree HEAD)"

files=$(printf '%s\n' "$listing" | grep -c . || true)
total=$(printf '%s\n' "$listing" | awk '{ s += $4 } END { print s + 0 }')

printf 'tracked files      %8d  (limit %d)\n' "$files" "$MAX_FILES"
printf 'tracked bytes      %8d  (limit %d)\n' "$total" "$MAX_TOTAL_BYTES"

# 1. File count.
[ "$files" -le "$MAX_FILES" ] ||
  fail "$files tracked files exceeds $MAX_FILES. A jump of this size is a directory nobody meant to add, not a feature"

# 2. Total size.
[ "$total" -le "$MAX_TOTAL_BYTES" ] ||
  fail "tracked content is $total bytes, over $MAX_TOTAL_BYTES. Check what was added before raising the limit"

echo 'repo hygiene: PASS'
