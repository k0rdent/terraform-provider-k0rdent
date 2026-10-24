#!/usr/bin/env bash
#
# Refuses three things in .github/ that hand a secret to code nobody reviewed,
# and a fourth that lets the vulnerability gate scan a Go the release doesn't
# ship.
#
# 1. A `uses:` that names a tag or a branch. A tag moves, so `@v4` runs whatever
#    its owner points it at on the day of the run, in a job that may hold the
#    App key or the signing key. Every action outside this repository is pinned
#    to a full commit, with the release it came from in a `# vX.Y.Z` comment.
# 2. An actions/checkout that leaves its token in .git/config, where every later
#    step can read it. There are no exceptions: a job that needs more history
#    asks the checkout for it with fetch-depth.
# 3. A signing job that leaves the keyring on the runner for the steps after it,
#    or that restores a Go cache. Another job wrote that cache, and the build
#    trusts a cache entry without rebuilding it, so whatever wrote it has a say
#    in what gets signed.
# 4. A release toolchain named anywhere but .go-release-version, or a `make
#    vulncheck` that doesn't scan under it. govulncheck reports standard
#    library advisories for the Go it runs under, so a gate run only under the
#    CI build version says nothing about the standard library the signed
#    binaries link.
# 5. A setup-terraform or setup-opentofu that doesn't name an exact version,
#    or two jobs that name different ones. Without a version they install
#    whatever shipped that day, so a run that turns red can't be told apart
#    from a new CLI, and two versions measure two things under one name.
#
# Usage: scripts/check-workflow-actions.sh [repository root]
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

problems=0
report() { echo "workflow actions: $*" >&2; problems=$((problems + 1)); }

# steps FILE CONDITION prints "LINE JOB" for each step in FILE where the awk
# expression CONDITION is true. LINE is the line the step's "- " is on. A step
# runs from that line to the next one indented no deeper than the dash, and
# CONDITION sees its text in `step`, with comments stripped, so a setting that
# only appears in a comment doesn't count. JOB is "-" outside a workflow's
# jobs. Every check below that looks at a step goes through this one parser.
# Most callers read it through process substitution, which hides a failure, so
# a parser that fails stops the whole run instead of reporting nothing.
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
while IFS= read -r f; do files+=("$f"); done < <(
  find "$root/.github/workflows" "$root/.github/actions" -type f \
    \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort
)
[ "${#files[@]}" -gt 0 ] || { report "no workflow files under $root/.github"; exit 1; }

for f in "${files[@]}"; do
  rel="${f#"$root"/}"

  # 1. Every non-local action is a 40-hex commit with a version comment.
  # The regular expression is bash's, not awk's: mawk on the runner has no {40}.
  # awk finds each `uses:` key, block or flow style, quoted or not, and prints
  # its value. A line that says uses followed by a colon in any other shape is
  # printed as unread and refused, so a layout this parser doesn't know fails
  # the check instead of slipping past it.
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
    (" " $0) ~ /[^A-Za-z0-9_-]["\047]?uses["\047]?[[:space:]]*:/ { print NR, "unread" }' "$f")

  # 2. Every checkout says persist-credentials: false. Comments don't count.
  while IFS=' ' read -r line job; do
    report "$rel:$line actions/checkout in job $job keeps its credentials. Add persist-credentials: false"
  done < <(steps "$f" '
    step ~ /uses["\047]?[[:space:]]*:[[:space:]]*["\047]?actions\/checkout@/ &&
    step !~ /persist-credentials["\047]?[[:space:]]*:[[:space:]]*["\047]?false/')
done

# 3. The signing job removes the keyring, always, before anything after it
#    runs. upload-artifact is the step that follows signing.
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

# A go-version other than the workflow's GO_VERSION is a second place a
# toolchain is decided, whether it's a number or a word like stable. The build
# floor comes from each workflow's GO_VERSION, and the release Go from the file
# through go-version-file.
for f in "${files[@]}"; do
  rel="${f#"$root"/}"
  while IFS=' ' read -r line value; do
    report "$rel:$line sets go-version to '$value'. Use \${{ env.GO_VERSION }} or go-version-file: .go-release-version"
  done < <(awk '
    /^[[:space:]]*#/ { next }
    {
      s = " " $0; sub(/[[:space:]]#.*$/, "", s)
      if (!match(s, /[^A-Za-z0-9_-]["\047]?go-version["\047]?[[:space:]]*:[[:space:]]*/)) next
      v = substr(s, RSTART + RLENGTH)
      if (v !~ /^["\047]?[$][{][{][[:space:]]*env\.GO_VERSION[[:space:]]*[}][}]["\047]?[[:space:]]*([,}].*)?$/) print NR, v
    }' "$f")
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
  for f in "${files[@]}"; do
    rel="${f#"$root"/}"
    while IFS=' ' read -r line _; do
      report "$rel:$line uses $action but doesn't pin $key to an exact version such as 1.2.3"
    done < <(steps "$f" '
      step ~ /uses["\047]?[[:space:]]*:[[:space:]]*["\047]?'"${action//\//\\/}"'@/ &&
      step !~ /'"$key"'["\047]?[[:space:]]*:[[:space:]]*'"$exact"'/')
    while IFS= read -r v; do versions+=("$v"); done < <(awk -v key="$key" '
      /^[[:space:]]*#/ { next }
      {
        s = $0; sub(/[[:space:]]#.*$/, "", s)
        if (!match(s, "^[[:space:]]*[\"\047]?" key "[\"\047]?[[:space:]]*:[[:space:]]*")) next
        v = substr(s, RLENGTH + 1); gsub(/[\"\047[:space:]]/, "", v); print v
      }' "$f")
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
