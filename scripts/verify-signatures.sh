#!/usr/bin/env bash
#
# Every commit in a range must be signed by whoever wrote it. Branch protection
# guards one branch on one host. This checks the commits themselves.
#
# It asks GitHub rather than running `git verify-commit`: a runner holds no
# keyring, so a local check answers `N` for a good signature.
set -euo pipefail

repo="${GITHUB_REPOSITORY:-k0rdent/terraform-provider-k0rdent}"
range="${1:-}"

if [ -z "$range" ]; then
  echo "usage: verify-signatures.sh <base>..<head>" >&2
  exit 2
fi

commits="$(git rev-list "$range")"
if [ -z "$commits" ]; then
  echo "signatures: nothing to check in $range"
  exit 0
fi

total=0
bad=0
while read -r sha; do
  [ -n "$sha" ] || continue
  total=$((total + 1))
  subject="$(git log -1 --format=%s "$sha")"
  author="$(git log -1 --format='%an' "$sha")"

  # `reason` tells "unsigned", "unknown_signature_type" and "unverified_email"
  # apart, which need different fixes.
  if ! answer="$(gh api "repos/$repo/commits/$sha" \
      --jq '"\(.commit.verification.verified) \(.commit.verification.reason)"' 2>&1)"; then
    # Usually a commit that hasn't been pushed yet.
    bad=$((bad + 1))
    echo "  $sha  not-on-remote  $author  $subject" >&2
    continue
  fi
  read -r verified reason <<<"$answer"

  if [ "$verified" != "true" ]; then
    bad=$((bad + 1))
    echo "::error title=Unsigned commit::$sha ($reason) by $author: $subject"
    echo "  $sha  $reason  $author  $subject" >&2
  fi
done <<<"$commits"

echo "signatures: $((total - bad)) of $total verified"
if [ "$bad" -gt 0 ]; then
  echo "signatures: FAIL - $bad commit(s) are not verified by GitHub against a key their author registered" >&2
  exit 1
fi
echo "signatures: PASS"
