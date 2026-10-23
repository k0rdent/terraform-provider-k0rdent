#!/usr/bin/env bash
# The gate has to refuse a tag that raises the major version past the latest
# published release, let every other release through, and say what is open
# when it refuses. A gate that blocks every v1 patch only teaches people to
# tick the override.
set -euo pipefail
cd "$(dirname "$0")/../.."
repo="$PWD"
gate="$repo/scripts/check-major-release.sh"
fail() { echo "major-release-gate: $*" >&2; exit 1; }

# allowed TAG WHY [PUBLISHED...]: the gate lets TAG through, given those
# published releases.
allowed() { local tag="$1" why="$2"; shift 2; "$gate" "$tag" "$@" >/dev/null 2>&1 || fail "$tag was refused ($why)"; }
refused() { local tag="$1" why="$2"; shift 2; "$gate" "$tag" "$@" >/dev/null 2>&1 && fail "$tag was allowed ($why)"; true; }

# Nothing published yet: 0.x is free, v1 is a decision. The dispatch passes
# one empty argument when there is no release at all.
allowed v0.1.0 "no release yet"
allowed v0.1.0 "no release yet, one empty argument" ""
refused v1.0.0 "v0 to v1 with no release"
refused v1.0.0 "v0 to v1 with no release, one empty argument" ""

published=(v0.1.0 v0.2.0-rc.1 v0.2.0)
for tag in v0.2.1 v0.3.0 v0.12.3 v0.3.0-rc1 v0.3.0-beta.1; do allowed "$tag" "0.x after 0.x" "${published[@]}"; done
for tag in v1.0.0 v1.0.0-rc.1 v2.0.0; do refused "$tag" "past the latest major, 0" "${published[@]}"; done

# A tag can't be its own precedent, even if it somehow shows up in the list.
refused v1.0.0 "v1.0.0 is its own only v1" "${published[@]}" v1.0.0

# Only well-formed versions count, so junk in the list opens nothing.
for junk in v1 v1.0 v01.0.0 v1.0.0+build "v1.0.0 " not-a-tag; do
  refused v1.0.0 "the only v1 in the list is $junk" "${published[@]}" "$junk"
done

out="$("$gate" v1.0.0 "${published[@]}" 2>&1 || true)"
for needle in "allow_major" "history" "0.x"; do
  printf '%s' "$out" | grep -Fq "$needle" || fail "the v1 refusal does not mention $needle"
done
printf '%s' "$out" | grep -Fq "RELEASE_ALLOW_MAJOR" && fail "the refusal names internal plumbing"

RELEASE_ALLOW_MAJOR=1 "$gate" v1.0.0 "${published[@]}" >/dev/null 2>&1 || fail "the override did not clear the gate"

# A published candidate opens its major: the override was ticked for it.
allowed v1.0.0 "v1.0.0-rc.1 is published" "${published[@]}" v1.0.0-rc.1

# Once v1 is published, v1 releases are routine.
published+=(v1.0.0)
for tag in v1.0.1 v1.2.3 v1.3.0-rc.1 v0.2.2; do allowed "$tag" "v1 is published" "${published[@]}"; done
for tag in v2.0.0 v2.0.0-rc.1; do refused "$tag" "v2 after v1" "${published[@]}"; done

out="$("$gate" v2.0.0 "${published[@]}" 2>&1 || true)"
printf '%s' "$out" | grep -Fq "allow_major" || fail "the v2 refusal does not say how to clear it"
printf '%s' "$out" | grep -Fq "BREAKING CHANGES" || fail "the v2 refusal does not name the changelog section"

# Order doesn't matter: the highest published major wins.
allowed v3.0.1 "v3 published before older ones in the list" v3.0.0 v1.0.0 v0.1.0

# Malformed tags are refused even with the override.
for tag in not-a-tag v1.2 v1.2.3+build.1 v1.2.3-rc.1+build v01.2.3 v1.02.3 v1.2.03 v1.2.3-01 "v1.2.3;id"; do
  RELEASE_ALLOW_MAJOR=1 "$gate" "$tag" >/dev/null 2>&1 && fail "malformed tag $tag was accepted"
done

cd "$repo"
# The workflow has to actually call it, with the published releases, or the
# script is decoration.
grep -Fq 'check-major-release.sh "$RELEASE_TAG" "${published_tags[@]}"' .github/workflows/release-dispatch.yml \
  || fail "the release workflow does not run the gate against the published releases"
grep -Fq 'allow_major' .github/workflows/release-dispatch.yml \
  || fail "the release workflow has no way to clear the gate"

echo "major-release-gate: ok"
