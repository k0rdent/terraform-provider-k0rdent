#!/usr/bin/env bash
#
# Refuses five things in .github/:
#
# 1. A `uses:` that names a tag or a branch. A tag moves, and the job may hold
#    the App key or the signing key. Pin a full commit with a `# vX.Y.Z` comment.
# 2. An actions/checkout that leaves its token in .git/config for every later
#    step. A job that needs more history asks for it with fetch-depth.
# 3. A signing job that leaves the keyring on the runner, or restores a Go
#    cache another job wrote, which the build trusts without rebuilding.
# 4. A release toolchain named anywhere but .go-release-version, or a `make
#    vulncheck` that doesn't scan under it. govulncheck only reports standard
#    library advisories for the Go it runs under.
# 5. A setup-terraform or setup-opentofu without an exact version, or two jobs
#    on different versions.
#
# Usage: scripts/check-workflow-actions.sh [repository root]
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

problems=0
report() { echo "workflow actions: $*" >&2; problems=$((problems + 1)); }

# steps FILE CONDITION prints "LINE JOB" for each step in FILE where the awk
# expression CONDITION is true. LINE is the line of the step's "- ", and the
# step runs to the next line indented no deeper than the dash. CONDITION sees
# the step's text, comments stripped, in `step`. JOB is "-" outside `jobs:`.
# Callers read it through process substitution, which hides a failure, so a
# parse failure kills the whole run.
steps() {
  local cond out
  cond="$(printf '%s' "$2" | tr '\n' ' ')"
  out="$(awk '
    function indent(s) { match(s, /^ */); return RLENGTH }
    function close_step() {
      if (open && ('"$cond"')) print start, job
      open = 0; step = ""
    }
    BEGIN { job = "-" }
    /^[[:space:]]*(#|$)/ { next }
    {
      n = indent($0)
      if (open && n <= dash) close_step()
      if ($0 ~ /^  [A-Za-z0-9_-]+:[[:space:]]*$/ && injobs) { job = $1; sub(/:$/, "", job) }
      if ($0 ~ /^jobs:/) injobs = 1
      if (!open && $0 ~ /^ *- /) { open = 1; dash = n; start = NR }
      s = $0; sub(/[[:space:]]#.*$/, "", s)
      if (open) step = step "\n" s
    }
    END { close_step() }' "$1")" || {
    echo "workflow actions: couldn't parse the steps in $1" >&2
    kill "$$"
  }
  [ -z "$out" ] || printf '%s\n' "$out"
}

# first LINES prints the line number from the first "LINE JOB" row, if any.
first() { local row="${1%%$'\n'*}"; printf '%s' "${row%% *}"; }

files=()
while IFS= read -r file; do files+=("$file"); done < <(
  find "$root/.github/workflows" "$root/.github/actions" -type f \
    \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort
)
[ "${#files[@]}" -gt 0 ] || { report "no workflow files under $root/.github"; exit 1; }

for file in "${files[@]}"; do
  rel="${file#"$root"/}"

  # 1. Every non-local action is a 40-hex commit with a version comment.
  # The regular expression is bash's, not awk's: mawk on the runner has no {40}.
  # awk prints each `uses:` value, block or flow style, quoted or not. Any other
  # line with a uses key is printed as unread and refused, so an unknown layout
  # fails instead of slipping past.
  pin='^[^@[:space:]]+@[0-9a-f]{40}[[:space:]]+#[[:space:]]*v[0-9]+\.[0-9]+\.[0-9]+([[:space:]]|$)'
  while IFS=' ' read -r line kind value; do
    if [ "$kind" = unread ]; then
      report "$rel:$line has a uses key this check can't read. Write it as '- uses: owner/repo@<commit>  # vX.Y.Z'"
      continue
    fi
    case "$value" in ./* | \"./* | \'./*) continue ;; esac
    [[ "$value" =~ $pin ]] ||
      report "$rel:$line uses $value, which is not a commit pin with a '# vX.Y.Z' comment"
  done < <(awk '
    /^[[:space:]]*#/ { next }
    match($0, /^[[:space:]]*(-[[:space:]]+)?([{][[:space:]]*)?["\047]?uses["\047]?[[:space:]]*:[[:space:]]*/) {
      lead = substr($0, 1, RLENGTH); v = substr($0, RLENGTH + 1)
      if (lead ~ /[{]/) sub(/[[:space:]]*[}].*$/, "", v)
      print NR, "value", v; next
    }
    (" " $0) ~ /[^A-Za-z0-9_-]["\047]?uses["\047]?[[:space:]]*:/ { print NR, "unread" }' "$file")

  # 2. Every checkout says persist-credentials: false. Comments don't count.
  while IFS=' ' read -r line job; do
    report "$rel:$line actions/checkout in job $job keeps its credentials. Add persist-credentials: false"
  done < <(steps "$file" '
    step ~ /uses["\047]?[[:space:]]*:[[:space:]]*["\047]?actions\/checkout@/ &&
    step !~ /persist-credentials["\047]?[[:space:]]*:[[:space:]]*["\047]?false/')
done

# 3. A job that holds the signing key always removes the keyring before
#    anything after it runs. In the release, upload-artifact follows signing.
check="$root/.github/workflows/release-signing-check.yml"
if [ -f "$check" ]; then
  if [ -z "$(first "$(steps "$check" 'job == "check-signing-key" &&
    step ~ /if:[[:space:]]*always\(\)/ &&
    step ~ /gpgconf --homedir "\$RUNNER_TEMP\/gnupg" --kill/ &&
    step ~ /rm -rf "\$RUNNER_TEMP\/gnupg"/')")" ]; then
    report "release-signing-check.yml check-signing-key has no 'if: always()' step that stops gpg-agent and removes \$RUNNER_TEMP/gnupg"
  fi
  if [ -z "$(first "$(steps "$check" 'job == "check-signing-key" &&
    step ~ /if:[[:space:]]*always\(\)/ &&
    step ~ /rm -f "\$RUNNER_TEMP\/gpg-passphrase"/')")" ]; then
    report "release-signing-check.yml check-signing-key has no 'if: always()' step that removes \$RUNNER_TEMP/gpg-passphrase"
  fi
fi

dispatch="$root/.github/workflows/release-dispatch.yml"
if [ -f "$dispatch" ]; then
  signing='job == "build-signed-artifacts"'
  # Steps are reported by the line their "- " is on, so the order of the
  # cleanup and the upload can be compared.
  cleanup="$(first "$(steps "$dispatch" "$signing"' &&
    step ~ /if:[[:space:]]*always\(\)/ &&
    step ~ /gpgconf --homedir "\$RUNNER_TEMP\/gnupg" --kill/ &&
    step ~ /rm -rf "\$RUNNER_TEMP\/gnupg"/')")"
  upload="$(first "$(steps "$dispatch" "$signing"' &&
    step ~ /uses["\047]?[[:space:]]*:[[:space:]]*["\047]?actions\/upload-artifact@/')")"

  if [ -z "$cleanup" ]; then
    report "release-dispatch.yml build-signed-artifacts has no 'if: always()' step that stops gpg-agent and removes \$RUNNER_TEMP/gnupg"
  elif [ -n "$upload" ] && [ "$cleanup" -gt "$upload" ]; then
    report "release-dispatch.yml build-signed-artifacts removes the keyring after upload-artifact runs"
  fi

  # setup-go caches unless told not to, so the setting has to be there.
  while IFS=' ' read -r line _; do
    report "release-dispatch.yml:$line build-signed-artifacts sets up Go with a cache. Add cache: false"
  done < <(steps "$dispatch" "$signing"' &&
    step ~ /uses["\047]?[[:space:]]*:[[:space:]]*["\047]?actions\/setup-go@/ &&
    step !~ /cache["\047]?[[:space:]]*:[[:space:]]*["\047]?false/')
fi

# 4. One release toolchain, and the vulnerability gate scans it.
release_go=""
[ -f "$root/.go-release-version" ] && release_go="$(tr -d '[:space:]' <"$root/.go-release-version")"
[[ "$release_go" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  report ".go-release-version is missing or doesn't hold one Go version such as 1.27.2"

# Any go-version but the workflow's GO_VERSION, a number or a word like
# stable, is a second place the toolchain is decided.
for file in "${files[@]}"; do
  rel="${file#"$root"/}"
  while IFS=' ' read -r line value; do
    report "$rel:$line sets go-version to '$value'. Use \${{ env.GO_VERSION }} or go-version-file: .go-release-version"
  done < <(awk '
    /^[[:space:]]*#/ { next }
    {
      s = " " $0; sub(/[[:space:]]#.*$/, "", s)
      if (!match(s, /[^A-Za-z0-9_-]["\047]?go-version["\047]?[[:space:]]*:[[:space:]]*/)) next
      v = substr(s, RSTART + RLENGTH)
      if (v !~ /^["\047]?[$][{][{][[:space:]]*env\.GO_VERSION[[:space:]]*[}][}]["\047]?[[:space:]]*([,}].*)?$/) print NR, v
    }' "$file")
done

# Every setup-go in the release workflow reads the file, and nothing else.
# setup-go prefers go-version over go-version-file when a step sets both.
if [ -f "$dispatch" ]; then
  while IFS=' ' read -r line _; do
    report "release-dispatch.yml:$line sets up Go without reading go-version-file: .go-release-version alone"
  done < <(steps "$dispatch" '
    step ~ /uses["\047]?[[:space:]]*:[[:space:]]*["\047]?actions\/setup-go@/ &&
    (step !~ /go-version-file["\047]?[[:space:]]*:[[:space:]]*["\047]?\.go-release-version/ ||
     step ~ /go-version["\047]?[[:space:]]*:/)')
fi

# make vulncheck runs govulncheck under the release Go as well as the CI one,
# and the CI vulncheck job runs make vulncheck.
recipe="$(awk '
  /^vulncheck:/ { on = 1; next }
  on && /^[^\t]/ { exit }
  on { print }' "$root/Makefile" 2>/dev/null || true)"

grep -Eq '^RELEASE_GO_VERSION[[:space:]]*[:?]?=.*\.go-release-version' "$root/Makefile" 2>/dev/null ||
  report "Makefile doesn't read RELEASE_GO_VERSION from .go-release-version"

printf '%s\n' "$recipe" | grep -Fq 'GOTOOLCHAIN=go$(RELEASE_GO_VERSION) go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)' ||
  report "make vulncheck doesn't scan under the release toolchain (GOTOOLCHAIN=go\$(RELEASE_GO_VERSION))"

awk '
  /^  vulncheck:/ { on = 1; next }
  on && /^  [A-Za-z0-9_-]+:/ { exit }
  on && /run:[[:space:]]*make vulncheck[[:space:]]*$/ { found = 1 }
  END { exit !found }' "$root/.github/workflows/ci.yml" 2>/dev/null ||
  report "ci.yml's vulncheck job doesn't run make vulncheck"

# 5. Terraform and OpenTofu pinned to one exact version each.
exact='["\047]?[0-9]+\.[0-9]+\.[0-9]+["\047]?([[:space:]]|$)'
for cli in terraform:hashicorp/setup-terraform tofu:opentofu/setup-opentofu; do
  key="${cli%%:*}_version" action="${cli#*:}"
  versions=()

  for file in "${files[@]}"; do
    rel="${file#"$root"/}"

    while IFS=' ' read -r line _; do
      report "$rel:$line uses $action but doesn't pin $key to an exact version such as 1.2.3"
    done < <(steps "$file" '
      step ~ /uses["\047]?[[:space:]]*:[[:space:]]*["\047]?'"${action//\//\\/}"'@/ &&
      step !~ /'"$key"'["\047]?[[:space:]]*:[[:space:]]*'"$exact"'/')

    while IFS= read -r version; do versions+=("$version"); done < <(awk -v key="$key" '
      /^[[:space:]]*#/ { next }
      {
        s = $0; sub(/[[:space:]]#.*$/, "", s)
        if (!match(s, "^[[:space:]]*[\"\047]?" key "[\"\047]?[[:space:]]*:[[:space:]]*")) next
        v = substr(s, RLENGTH + 1); gsub(/[\"\047[:space:]]/, "", v); print v
      }' "$file")
  done

  if [ "${#versions[@]}" -gt 0 ] && [ "$(printf '%s\n' "${versions[@]}" | sort -u | wc -l)" -gt 1 ]; then
    report "the workflows pin more than one ${cli%%:*} version: $(printf '%s\n' "${versions[@]}" | sort -u | tr '\n' ' ')"
  fi
done

if [ "$problems" -gt 0 ]; then
  echo "workflow actions: $problems problem(s)" >&2
  exit 1
fi
echo "workflow actions: every action is pinned, every checkout drops its token, the signing job keeps no keyring and restores no cache, vulncheck scans the release Go, terraform and tofu are pinned"
