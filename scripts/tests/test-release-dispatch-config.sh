#!/usr/bin/env bash
# The release workflows are checked by validateProtectedReleaseSigning in
# internal/repocheck. This checks the one release guard that doesn't read: the
# README's dev_overrides selector.
set -euo pipefail

cd "$(dirname "$0")/../.."

fail() {
  echo "release dispatch config: $*" >&2
  exit 1
}

grep -Fq '    "registry.terraform.io/k0rdent/k0rdent" =' README.md ||
  fail "README dev_overrides selector is not canonical"

echo "release dispatch config: PASS"
