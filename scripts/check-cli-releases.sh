#!/usr/bin/env bash
#
# Fails when ci.yml's acceptance matrices aren't the two newest releases of
# Terraform and of OpenTofu: for each, the newest patch of the newest minor
# and of the minor before it. TESTING.md has the rule and how to move the
# pins. It reads releases.hashicorp.com and get.opentofu.org, so the nightly
# runs it, not every pull request.
#
# Usage: scripts/check-cli-releases.sh [terraform-index.json tofu-index.json]
#   The two release indexes, in each project's own format. Fetched when left
#   out.
#   CI_WORKFLOW: the workflow holding the matrices. ci.yml when unset.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ci="${CI_WORKFLOW:-$root/.github/workflows/ci.yml}"

problems=0
report() { echo "cli releases: $*" >&2; problems=$((problems + 1)); }

if [ $# -ne 0 ] && [ $# -ne 2 ]; then
  echo "usage: $0 [terraform-index.json tofu-index.json]" >&2
  exit 2
fi

# check NAME KEY URL FILTER FILE compares ci.yml's `KEY: [...]` matrix with
# the two newest releases in the index at URL, or in FILE when it's set.
# FILTER is the jq that lists the index's version names.
check() {
  local name="$1" key="$2" url="$3" filter="$4" file="$5" index want have
  if [ -n "$file" ]; then
    index="$(cat "$file")"
  else
    index="$(curl -fsSL --retry 3 "$url")" || { report "couldn't read $url"; return; }
  fi

  # The newest patch of each minor, in version order, then the last two.
  want="$(printf '%s' "$index" |
    jq -r "$filter" 2>/dev/null |
    grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' |
    sort -V |
    awk -F. '{ m = $1 "." $2; if (prev != "" && m != prev) print last; prev = m; last = $0 }
      END { if (last != "") print last }' |
    tail -2 | paste -sd' ' -)" || true
  [ -n "$want" ] || { report "no stable releases in $name's release index"; return; }

  have="$(sed -n "s/^[[:space:]]*$key:[[:space:]]*\[\(.*\)\].*/\1/p" "$ci" | head -1 |
    tr -d "\"' " | tr ',' '\n' | sort -V | paste -sd' ' -)"
  [ -n "$have" ] || { report "no $key: [...] matrix in $ci"; return; }

  if [ "$have" != "$want" ]; then
    report "$name's two newest releases are $want, and ci.yml's acceptance matrix tests $have. Move the pins as TESTING.md says: the matrix to both, every other ${key}_version to the newer one."
    return
  fi
  echo "cli releases: ci.yml tests $have, $name's two newest releases"
}

check Terraform terraform https://releases.hashicorp.com/terraform/index.json \
  '.versions | keys[]' "${1:-}"
check OpenTofu tofu https://get.opentofu.org/tofu/api.json \
  '.versions[].id' "${2:-}"

[ "$problems" -eq 0 ] || exit 1
