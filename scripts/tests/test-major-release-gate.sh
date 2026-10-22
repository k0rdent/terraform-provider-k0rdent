#!/usr/bin/env bash
# The gate has to refuse a tag that raises the major version past the latest
# release on main, let every other release through, and say what is open when
# it refuses. A gate that blocks every v1 patch only teaches people to tick the
# override.
set -euo pipefail
cd "$(dirname "$0")/../.."
repo="$PWD"
gate="$repo/scripts/check-major-release.sh"
fail() { echo "major-release-gate: $*" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cd "$work"
git init -q -b main
g() { git -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false -c tag.gpgsign=false "$@"; }
commit() { g commit -q --allow-empty -m "$1"; }
commit first

allowed() { "$gate" "$1" main >/dev/null 2>&1 || fail "$1 was refused ($2)"; }
refused() { "$gate" "$1" main >/dev/null 2>&1 && fail "$1 was allowed ($2)"; true; }

# Nothing released yet: 0.x is free, v1 is a decision.
allowed v0.1.0 "no release yet"
refused v1.0.0 "v0 to v1 with no release"

g tag v0.1.0; commit second; g tag v0.2.0
for tag in v0.2.1 v0.3.0 v0.12.3 v0.3.0-rc1 v0.3.0-beta.1; do allowed "$tag" "0.x after 0.x"; done
for tag in v1.0.0 v1.0.0-rc.1 v2.0.0; do refused "$tag" "past the latest major, 0"; done

# The tag under test is already on main when the dispatch runs. It must not
# count as its own precedent.
commit third; g tag v1.0.0
refused v1.0.0 "v1.0.0 is its own only v1"

out="$("$gate" v1.0.0 main 2>&1 || true)"
for needle in "allow_major" "history" "0.x"; do
  printf '%s' "$out" | grep -Fq "$needle" || fail "the v1 refusal does not mention $needle"
done
printf '%s' "$out" | grep -Fq "RELEASE_ALLOW_MAJOR" && fail "the refusal names internal plumbing"

RELEASE_ALLOW_MAJOR=1 "$gate" v1.0.0 main >/dev/null 2>&1 || fail "the override did not clear the gate"

# Once v1 is released, v1 releases are routine.
commit fourth
for tag in v1.0.1 v1.2.3 v1.3.0-rc.1 v0.2.2; do allowed "$tag" "v1 is released"; done
for tag in v2.0.0 v2.0.0-rc.1; do refused "$tag" "v2 after v1"; done

out="$("$gate" v2.0.0 main 2>&1 || true)"
printf '%s' "$out" | grep -Fq "allow_major" || fail "the v2 refusal does not say how to clear it"

# A major released on another branch is not a precedent for main.
g checkout -q -b side; commit side; g tag v3.0.0; g checkout -q main
refused v3.1.0 "v3 exists only off main"

# Malformed tags are refused even with the override.
for tag in not-a-tag v1.2 v1.2.3+build.1 v1.2.3-rc.1+build v01.2.3 v1.02.3 v1.2.03 v1.2.3-01 "v1.2.3;id"; do
  RELEASE_ALLOW_MAJOR=1 "$gate" "$tag" main >/dev/null 2>&1 && fail "malformed tag $tag was accepted"
done

"$gate" v0.2.3 no-such-ref >/dev/null 2>&1 && fail "an unresolvable main ref was accepted"

cd "$repo"
# The workflow has to actually call it, against main, or the script is decoration.
grep -Eq 'check-major-release\.sh "\$RELEASE_TAG" origin/main' .github/workflows/release-dispatch.yml \
  || fail "the release workflow does not run the gate against main"
grep -Fq 'allow_major' .github/workflows/release-dispatch.yml \
  || fail "the release workflow has no way to clear the gate"

echo "major-release-gate: ok"
