#!/usr/bin/env bash
#
# Fail if any tracked file matches a .gitignore rule.
#
# Such a file is almost always a cache added with `git add -f` or before the
# rule existed. Go caches got into history this way once, at ~48k files and
# >1.5 GB, and had to be rewritten out.
#
# Fix: `git rm -r --cached <path>` and commit, or remove the ignore rule if the
# file really belongs in the repo.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

tracked_ignored="$(git ls-files --cached --ignored --exclude-standard)"

if [ -z "$tracked_ignored" ]; then
  echo "ok: no tracked file matches a .gitignore rule"
  exit 0
fi

count="$(printf '%s\n' "$tracked_ignored" | wc -l | tr -d ' ')"
echo "::error title=tracked files match .gitignore::${count} tracked file(s) match an ignore rule. Remove them with 'git rm -r --cached <path>' or drop the rule."
echo
# awk rather than head: head exiting early on a long list sends SIGPIPE, and
# set -e turns that into exit 141 before the exit 1.
echo "By directory (top two path components):"
printf '%s\n' "$tracked_ignored" | cut -d/ -f1-2 | sort | uniq -c | sort -rn | awk 'NR <= 20'
echo
echo "First files:"
printf '%s\n' "$tracked_ignored" | awk 'NR <= 20'
exit 1
