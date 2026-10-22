#!/usr/bin/env bash
#
# Fail if any tracked file matches a .gitignore rule.
#
# A path that is both ignored and tracked is almost always a build artifact or
# cache that was added with `git add -f` or before the ignore rule existed. Go
# module, build and lint caches got into history this way once, at ~48k files
# and >1.5 GB, and had to be rewritten out.
# CI runs this on every push and pull request so it cannot happen again.
#
# Fix: `git rm -r --cached <path>` and commit, or remove the ignore rule if the
# file really belongs in the repo.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# --cached restricts to the index, --ignored --exclude-standard applies
# .gitignore, .git/info/exclude and core.excludesFile. Directories are listed
# per file, so collapse to the top two path components for a readable report.
# The report uses awk rather than head: with tens of thousands of offenders,
# head exiting early sends SIGPIPE up the pipeline and set -e turns that into
# exit 141 before the final exit 1 runs.
tracked_ignored="$(git ls-files --cached --ignored --exclude-standard)"

if [ -z "$tracked_ignored" ]; then
  echo "ok: no tracked file matches a .gitignore rule"
  exit 0
fi

count="$(printf '%s\n' "$tracked_ignored" | wc -l | tr -d ' ')"
echo "::error title=tracked files match .gitignore::${count} tracked file(s) match an ignore rule. Remove them with 'git rm -r --cached <path>' or drop the rule."
echo
echo "By directory (top two path components):"
printf '%s\n' "$tracked_ignored" | cut -d/ -f1-2 | sort | uniq -c | sort -rn | awk 'NR <= 20'
echo
echo "First files:"
printf '%s\n' "$tracked_ignored" | awk 'NR <= 20'
exit 1
