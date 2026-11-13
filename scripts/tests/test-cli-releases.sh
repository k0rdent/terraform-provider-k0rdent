#!/usr/bin/env bash
#
# Proves scripts/check-cli-releases.sh passes matrices of the two newest
# Terraform and OpenTofu releases and refuses one that's behind, after a new
# patch or a new minor. It reads release indexes from files, so nothing here
# touches the network.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
check="$here/scripts/check-cli-releases.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "cli releases test: FAIL - $1" >&2; exit 1; }

# matrix TERRAFORM TOFU writes a ci.yml whose two matrices hold those lists.
matrix() {
  printf '    strategy:\n      matrix:\n        terraform: [%s]\n' "$1" > "$work/ci.yml"
  printf '    strategy:\n      matrix:\n        tofu: [%s]\n' "$2" >> "$work/ci.yml"
}
export CI_WORKFLOW="$work/ci.yml"

# terraform VERSION... writes HashiCorp's index format: versions keyed by name.
terraform() {
  printf '%s\n' "$@" | jq -R . | jq -s 'map({(.): {}}) | add | {name: "terraform", versions: .}' > "$work/terraform.json"
}

# tofu VERSION... writes OpenTofu's index format: a list of {id, files}.
tofu() {
  printf '%s\n' "$@" | jq -R . | jq -s '{versions: map({id: ., files: []})}' > "$work/tofu.json"
}

expect_pass() {
  "$check" "$work/terraform.json" "$work/tofu.json" > "$work/out" 2>&1 || { cat "$work/out" >&2; fail "$1"; }
}

expect_fail() {
  if "$check" "$work/terraform.json" "$work/tofu.json" > "$work/out" 2>&1; then
    cat "$work/out" >&2
    fail "$1"
  fi
  grep -Fq "$2" "$work/out" || { cat "$work/out" >&2; fail "$1: message did not mention $2"; }
}

matrix '"1.16.7", "1.17.1"' '"1.12.12", "1.13.9"'

# Older minors and pre-releases of the next one don't count.
terraform 1.15.9 1.16.6 1.16.7 1.17.0 1.17.1 1.18.0-alpha20261104 1.18.0-rc1
tofu 1.11.6 1.12.11 1.12.12 1.13.9 1.14.0-beta1
expect_pass "the two newest releases were refused"

# Version order, not text order: 1.16.10 is newer than 1.16.9.
matrix '"1.16.10", "1.17.1"' '"1.12.12", "1.13.9"'
terraform 1.16.9 1.16.10 1.17.1
expect_pass "1.16.10 wasn't taken as newer than 1.16.9"
matrix '"1.16.7", "1.17.1"' '"1.12.12", "1.13.9"'

terraform 1.16.7 1.17.0 1.17.1 1.17.2
expect_fail "a terraform matrix behind a new patch was accepted" "Terraform's two newest releases are 1.16.7 1.17.2"

terraform 1.16.7 1.17.1 1.18.0
expect_fail "a terraform matrix behind a new minor was accepted" "Terraform's two newest releases are 1.17.1 1.18.0"

terraform 1.18.0-rc1
expect_fail "a terraform index with no stable release was accepted" "no stable releases in Terraform's"

# OpenTofu, in its own index format, with Terraform current.
terraform 1.16.7 1.17.1
tofu 1.12.12 1.13.9 1.13.10
expect_fail "a tofu matrix behind a new patch was accepted" "OpenTofu's two newest releases are 1.12.12 1.13.10"

tofu 1.12.12 1.13.9 1.14.0
expect_fail "a tofu matrix behind a new minor was accepted" "OpenTofu's two newest releases are 1.13.9 1.14.0"

# Both behind: each is reported, so the first doesn't hide the second.
terraform 1.16.7 1.17.1 1.18.0
expect_fail "a terraform failure hid the tofu one" "OpenTofu's two newest releases are 1.13.9 1.14.0"
grep -Fq "Terraform's two newest releases are 1.17.1 1.18.0" "$work/out" || fail "a tofu failure hid the terraform one"

terraform 1.16.7 1.17.1
tofu 1.12.12 1.13.9
printf 'jobs: {}\n' > "$work/ci.yml"
expect_fail "a workflow with no terraform matrix was accepted" "no terraform: [...] matrix"
grep -Fq "no tofu: [...] matrix" "$work/out" || fail "a workflow with no tofu matrix was accepted"

echo "cli releases test: PASS"
