#!/usr/bin/env bash
#
# Runs scripts/check-workflow-actions.sh against this repository's .github/,
# then proves each of its refusals still fires, by copying .github/, the
# Makefile and .go-release-version, and undoing one fix at a time. A gate that
# passes everything looks the same as one that works until the day it's needed.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
gate="$here/scripts/check-workflow-actions.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "workflow actions test: FAIL - $1" >&2; exit 1; }

"$gate" "$here" || fail "this repository's .github/ does not pass"

setup() {
  rm -rf "$work/repo" && mkdir -p "$work/repo" && cp -R "$here/.github" "$work/repo/.github"
  cp "$here/Makefile" "$work/repo/Makefile"
  if [ -f "$here/.go-release-version" ]; then cp "$here/.go-release-version" "$work/repo/.go-release-version"; fi
}

# edit FILE PERL: a perl substitution run over the whole file at once (-0), so
# a pattern can span lines. perl, because sed -i differs between GNU and BSD.
# FILE is relative to .github/. edit_root takes one relative to the repository
# root.
edit() { perl -0pi -e "$2" "$work/repo/.github/$1"; }
edit_root() { perl -0pi -e "$2" "$work/repo/$1"; }

expect_fail() {
  if "$gate" "$work/repo" >"$work/out" 2>&1; then
    cat "$work/out" >&2
    fail "$1"
  fi
  grep -Fq "$2" "$work/out" || { cat "$work/out" >&2; fail "$1: message did not mention $2"; }
}

setup
edit workflows/ci.yml 's{uses: actions/setup-go\@[0-9a-f]{40}\s+# v[0-9.]+}{uses: actions/setup-go\@v5}'
expect_fail "a tag pin was accepted" "uses actions/setup-go@v5"

# The same tag pin in a flow mapping, behind a quoted key, and in a shape the
# parser doesn't know, which it has to refuse instead of skipping.
setup
edit workflows/ci.yml 's{- uses: actions/setup-go\@[0-9a-f]{40}\s+# v[0-9.]+}{- { uses: actions/setup-go\@v5 }}'
expect_fail "a tag pin in a flow mapping was accepted" "uses actions/setup-go@v5"

setup
edit workflows/nightly.yml 's{- uses: actions/setup-go\@[0-9a-f]{40}\s+# v[0-9.]+}{- "uses": evil/action\@main}'
expect_fail "a tag pin behind a quoted key was accepted" "uses evil/action@main"

setup
edit workflows/nightly.yml 's{- uses: actions/setup-go\@[0-9a-f]{40}\s+# v[0-9.]+}{- { name: Go, uses: evil/action\@main }}'
expect_fail "a uses key the parser can't read was accepted" "has a uses key this check can't read"

setup
edit workflows/ci.yml 's{(uses: hashicorp/setup-terraform\@[0-9a-f]{40})\s+# v[0-9.]+}{$1}'
expect_fail "a commit pin with no version comment was accepted" "hashicorp/setup-terraform@"

setup
edit workflows/nightly.yml 's{\n\s+persist-credentials: false\n}{\n}'
expect_fail "a checkout that keeps its token was accepted" "keeps its credentials"

# The release tag validation was the one job allowed to keep its token. It no
# longer is: its checkout fetches the history it needs up front.
setup
edit workflows/release-dispatch.yml 's{(ref: main\n\s+fetch-depth: 0)\n\s+persist-credentials: false\n}{$1\n}'
expect_fail "the tag validation's checkout kept its token" "in job validate-release-tag keeps its credentials"

# The setting only in a comment, which YAML ignores.
setup
edit workflows/nightly.yml 's{\n(\s+)persist-credentials: false\n}{\n$1fetch-depth: 0  # persist-credentials: false\n}'
expect_fail "a checkout with persist-credentials: false only in a comment was accepted" "keeps its credentials"

setup
edit workflows/release-dispatch.yml 's{rm -rf "\$RUNNER_TEMP/gnupg"}{true}'
expect_fail "a signing job that keeps its keyring was accepted" "removes \$RUNNER_TEMP/gnupg"

# The directory removed, with gpg-agent left running and holding the key.
setup
edit workflows/release-dispatch.yml 's{\n[^\n]*gpgconf --homedir "\$RUNNER_TEMP/gnupg" --kill[^\n]*}{}'
expect_fail "a signing job that leaves gpg-agent running was accepted" "stops gpg-agent"

# The cleanup step moved below the upload, which runs with the key still there.
setup
edit workflows/release-dispatch.yml 's{(      - name: Remove the signing keyring\n.*?rm -rf "\$RUNNER_TEMP/gnupg"\n)(.*)\z}{$2$1}s'
expect_fail "a keyring removed after upload-artifact was accepted" "after upload-artifact runs"

# The signing check holds the same key, so it has to clean up the same way.
setup
edit workflows/release-signing-check.yml 's{rm -rf "\$RUNNER_TEMP/gnupg"}{true}'
expect_fail "a signing check that keeps its keyring was accepted" "check-signing-key has no 'if: always()' step that stops gpg-agent"

setup
edit workflows/release-signing-check.yml 's{\n[^\n]*gpgconf --homedir "\$RUNNER_TEMP/gnupg" --kill[^\n]*}{}'
expect_fail "a signing check that leaves gpg-agent running was accepted" "check-signing-key has no 'if: always()' step that stops gpg-agent"

# The cleanup only when the check passed, which is when it matters least.
setup
edit workflows/release-signing-check.yml 's{(- name: Remove the signing keyring\n\s+)if: always\(\)}{${1}if: success()}'
expect_fail "a signing check whose cleanup can be skipped was accepted" "check-signing-key has no 'if: always()' step that stops gpg-agent"

setup
edit workflows/release-signing-check.yml 's{rm -f "\$RUNNER_TEMP/gpg-passphrase"}{true}'
expect_fail "a signing check that keeps the passphrase file was accepted" "removes \$RUNNER_TEMP/gpg-passphrase"

# The signing job restoring a module and build cache another job wrote, both
# by saying so and by leaving setup-go's default on.
setup
edit workflows/release-dispatch.yml 's{(go-version-file: \.go-release-version\n\s+cache:) false}{$1 true}'
expect_fail "a signing job that restores a cache was accepted" "sets up Go with a cache"

setup
edit workflows/release-dispatch.yml 's{(go-version-file: \.go-release-version)\n\s+cache: false\n}{$1\n}'
expect_fail "a signing job on setup-go's default cache was accepted" "sets up Go with a cache"

# The release toolchain written into a CI job, where it can drift from the file.
setup
edit workflows/ci.yml 's{go-version-file: \.go-release-version}{go-version: "1.27.2"}'
expect_fail "a literal release Go version was accepted" "sets go-version to '\"1.27.2\"'"

# A word in place of a number decides the toolchain just as much.
setup
edit workflows/nightly.yml 's{go-version: \$\{\{ env.GO_VERSION \}\}}{go-version: stable}'
expect_fail "go-version: stable was accepted" "sets go-version to 'stable'"

# The signing job back on its own GO_VERSION.
setup
edit workflows/release-dispatch.yml 's{go-version-file: \.go-release-version}{go-version: \$\{\{ env.GO_VERSION \}\}}'
expect_fail "a release build that doesn't read .go-release-version was accepted" "without reading go-version-file"

setup
printf 'stable\n' >"$work/repo/.go-release-version"
expect_fail "a release version file without a version was accepted" ".go-release-version is missing"

# The bug this check exists for: vulncheck under the CI Go alone.
setup
edit_root Makefile 's{^\tGOTOOLCHAIN=go\$\(RELEASE_GO_VERSION\) go run golang.org/x/vuln[^\n]*\n}{}m'
expect_fail "a vulncheck that skips the release toolchain was accepted" "doesn't scan under the release toolchain"

# An unpinned CLI. setup-terraform and setup-opentofu install the latest
# release when no version is named, so a run is measured against whatever
# shipped that day, and a red nightly can't be told from a new CLI.
setup
edit workflows/nightly.yml 's{\n\s+terraform_version: "[^"]*"}{}'
expect_fail "a setup-terraform with no version was accepted" "doesn't pin terraform_version"

setup
edit workflows/nightly.yml 's{\n\s+tofu_version: "[^"]*"}{}'
expect_fail "a setup-opentofu with no version was accepted" "doesn't pin tofu_version"

# A range is no pin either.
setup
edit workflows/ci.yml 's{terraform_version: "[0-9][^"]*"}{terraform_version: "~1.16"}'
expect_fail "a terraform_version range was accepted" "doesn't pin terraform_version"

# A matrix entry that's a range is no pin either.
setup
edit workflows/ci.yml 's{terraform: \["1\.16\.\d+"}{terraform: ["1.16.*"}'
expect_fail "a terraform matrix range was accepted" 'lists terraform "1.16.*"'

# Two versions under one check name measure two things and report one.
setup
edit workflows/ci.yml 's{name: acceptance under Terraform \$\{\{ matrix\.terraform \}\}}{name: acceptance}'
expect_fail "a matrix check name without its version was accepted" "doesn't show"

# Only the two newest minors, so a third version or a gap between them is
# testing a release HashiCorp no longer fixes.
setup
edit workflows/ci.yml 's{terraform: \["1\.16\.\d+",}{terraform: ["1.15.9", "1.16.7",}'
expect_fail "three terraform versions were accepted" "TESTING.md allows two"

setup
edit workflows/ci.yml 's{terraform: \["1\.16\.\d+"}{terraform: ["1.15.9"}'
expect_fail "terraform versions two minors apart were accepted" "aren't two adjacent minor releases"

setup
edit workflows/ci.yml 's{terraform: \["1\.16\.\d+"}{terraform: ["1.17.0"}'
expect_fail "two patches of one minor were accepted" "aren't two adjacent minor releases"

# A job outside the matrix on the older release tests what the matrix
# already covers and skips the newer one.
setup
edit workflows/nightly.yml 's{terraform_version: "[^"]*"}{terraform_version: "1.16.7"}'
expect_fail "a job outside the matrix on the older release was accepted" "not the newer release"

# OpenTofu follows the same rule, with its own matrix.
setup
edit workflows/ci.yml 's{tofu: \["1\.12\.\d+"}{tofu: ["1.12.*"}'
expect_fail "a tofu matrix range was accepted" 'lists tofu "1.12.*"'

setup
edit workflows/ci.yml 's{name: acceptance under OpenTofu \$\{\{ matrix\.tofu \}\}}{name: acceptance under OpenTofu}'
expect_fail "a tofu matrix check name without its version was accepted" "doesn't show \${{ matrix.tofu }}"

setup
edit workflows/ci.yml 's{tofu: \["1\.12\.\d+",}{tofu: ["1.11.6", "1.12.12",}'
expect_fail "three tofu versions were accepted" "pin 3 tofu versions"

setup
edit workflows/ci.yml 's{tofu: \["1\.12\.\d+"}{tofu: ["1.11.6"}'
expect_fail "tofu versions two minors apart were accepted" "pin tofu 1.11.6 and 1.13.9, which aren't two adjacent"

setup
edit workflows/nightly.yml 's{tofu_version: "[^"]*"}{tofu_version: "1.12.12"}'
expect_fail "a tofu job outside the matrix on the older release was accepted" "pins tofu 1.12.12, not the newer release"

echo "workflow actions test: PASS"
