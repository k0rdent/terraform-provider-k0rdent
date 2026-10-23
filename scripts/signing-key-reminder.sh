#!/usr/bin/env bash
#
# Keeps one open issue while the release signing key is due to rotate, so the
# reminder reaches a person and not just a nightly log. The nightly's
# signing-key job runs scripts/check-signing-key.sh with SIGNING_KEY_DUE_FILE,
# and this reads that file.
#
#   signing-key-reminder.sh DUE_FILE
#
# - Something due, none of the workflow's own issues open: opens one.
# - Something due, an issue open: edits its title and body to the latest, and
#   comments only when the title changed, so a due key is one notification,
#   not one a night.
# - Nothing due and the check passed (RESULT=success): closes the open issue.
#   Any other result says nothing, since a check that broke proves nothing
#   about the key.
#
# The issue is found by its label, never by its title, so there's never a
# second one. Only an issue this workflow opened is managed: anyone who can
# label can put the label on an issue, and that issue is never edited or
# closed. If several carry the label, the oldest of the workflow's own is
# managed, and each other one gets a single comment pointing to it.
# Needs GH_TOKEN with issues: write, and GH_REPO.
set -euo pipefail

due_file="${1:?usage: signing-key-reminder.sh DUE_FILE}"
label="signing-key-rotation"
bot="github-actions[bot]"
pointer="<!-- signing-key-rotation pointer -->"

# Open issues with the label, oldest first: number, then author. Pull
# requests come back from this endpoint too, so they're dropped.
labeled="$(gh api "repos/$GH_REPO/issues?state=open&labels=$label&sort=created&direction=asc&per_page=100" \
  --jq '.[] | select(.pull_request | not) | "\(.number)\t\(.user.login)"')"
number="$(awk -F'\t' -v bot="$bot" '$2 == bot { print $1; exit }' <<<"$labeled")"

# point_others: one comment on every other labeled issue, naming the managed
# one. Never twice, and never an edit or a close.
point_others() {
  local other seen
  while IFS=$'\t' read -r other _; do
    [ -n "$other" ] && [ "$other" != "$number" ] || continue
    seen="$(gh api "repos/$GH_REPO/issues/$other/comments?per_page=100" \
      --jq "[.[] | select(.user.login == \"$bot\" and (.body | contains(\"$pointer\")))] | length")"
    [ "$seen" = 0 ] || continue
    gh issue comment "$other" --body "The signing key rotation is tracked in #$number, which the nightly keeps up to date. This issue isn't changed by it. $pointer"
  done <<<"$labeled"
}

due=""
[ -f "$due_file" ] && due="$(grep -v '^[[:space:]]*$' "$due_file" || true)"

if [ -z "$due" ]; then
  if [ "${RESULT:-}" = success ] && [ -n "$number" ]; then
    gh issue close "$number" --comment "Rotated: the signing key check finds nothing due. ${RUN_URL:-}"
  fi
  exit 0
fi

# The title names what's due and the earliest date, the body every key.
whats="$(cut -f1 <<<"$due" | sed 's/^signing subkey$/subkey/' | sort -u | paste -sd ',' - | sed 's/,/ and /')"
earliest="$(cut -f3 <<<"$due" | sort | head -n 1)"
title="Rotate the release signing $whats (due $earliest)"
body="$(
  echo "The release signing key is due to rotate."
  echo
  while IFS=$'\t' read -r what fpr rotate expiry; do
    echo "- **$what** \`$fpr\`: rotate by $rotate, expires $expiry"
  done <<<"$due"
  echo
  echo "How to rotate: the maintainers' runbook, kept with the key. After rotating, commit the new public key to \`docs/signing-key.asc\` and run **Check release signing**, as [docs/release-signing.md](docs/release-signing.md) says."
  echo
  echo "This issue closes itself once the nightly's signing key check finds nothing due.${RUN_URL:+ Latest run: $RUN_URL}"
)"

if [ -z "$number" ]; then
  gh label create "$label" --color FBCA04 --description "The release signing key is due to rotate" >/dev/null 2>&1 || true
  url="$(gh issue create --title "$title" --label "$label" --body "$body")"
  echo "$url"
  number="${url##*/}"
else
  before="$(gh issue view "$number" --json title --jq '.title')"
  gh issue edit "$number" --title "$title" --body "$body"
  [ "$before" = "$title" ] || gh issue comment "$number" --body "Now due: $title. ${RUN_URL:-}"
fi
point_others
