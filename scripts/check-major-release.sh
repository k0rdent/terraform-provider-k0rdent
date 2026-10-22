#!/usr/bin/env bash
#
# Refuses a tag whose major version is higher than any release on main, until
# somebody decides to make it.
#
# A new major is a compatibility claim to every user, and v1 is also the point
# at which the repository has to be public for the registry to serve it. Those
# are decisions, not steps. A release within an already released major is
# routine and passes.
#
# The release dispatch's `allow_major` input clears it, and reaches this script
# as RELEASE_ALLOW_MAJOR=1.
#
#   check-major-release.sh <tag> [<main ref>]
set -euo pipefail

tag="${1:?usage: check-major-release.sh <tag> [<main ref>]}"
main_ref="${2:-origin/main}"

# Strict SemVer with an optional prerelease and no build metadata, as the
# release dispatch accepts.
num='(0|[1-9][0-9]*)'
ident='(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)'
semver="^v${num}\\.${num}\\.${num}(-${ident}(\\.${ident})*)?$"

if [[ ! "$tag" =~ $semver ]]; then
  printf 'check-major-release: %q is not vMAJOR.MINOR.PATCH with an optional prerelease\n' "$tag" >&2
  exit 1
fi
major="${BASH_REMATCH[1]}"

if ! git rev-parse --verify --quiet "${main_ref}^{commit}" >/dev/null; then
  printf 'check-major-release: cannot resolve %q to a commit\n' "$main_ref" >&2
  exit 1
fi

latest=0
while IFS= read -r released; do
  if [ "$released" != "$tag" ] && [[ "$released" =~ $semver ]] &&
    [ "${BASH_REMATCH[1]}" -gt "$latest" ]; then
    latest="${BASH_REMATCH[1]}"
  fi
done < <(git tag --list 'v*' --merged "$main_ref")

if [ "$major" -le "$latest" ]; then
  exit 0
fi
if [ "${RELEASE_ALLOW_MAJOR:-}" = "1" ]; then
  echo "check-major-release: ${tag} cleared by the allow_major input"
  exit 0
fi

echo "::error title=Major version release is gated::${tag} raises the major version from ${latest} to ${major}. It needs a decision, not a tag." >&2

# Printed rather than linked, because the person holding the tag is the person
# who needs the list.
if [ "$latest" -eq 0 ]; then
  cat >&2 <<'EOF'

Still open, and each one is harder to undo after the publish than before it:

  1. The upstream API is 0.x. A v1 provider claims a compatibility promise
     over an interface that has not made one. Staying 0.x is the segment
     norm, not a stigma.

  2. Going public exposes the history, not only the working tree.
     Deleting a file today does not remove it from the log. main is small
     and has been rewritten before, so this is minutes of work, but it
     has to happen before the repository is public and not after.

  3. The registry has no alias and no unpublish. This is the one
     irreversible step in the sequence.
EOF
else
  cat >&2 <<'EOF'

A new major tells every user their configuration may break. Before it ships,
CHANGELOG.md needs this version's BREAKING CHANGES section, rendered from the
breaking-change entries in .changelog/, and docs/public-compatibility.md the
migration for each line in it. The registry has no unpublish.
EOF
fi

cat >&2 <<'EOF'

When that is settled, re-run the Dispatch signed provider release workflow
with the allow_major input ticked.
EOF
exit 1
