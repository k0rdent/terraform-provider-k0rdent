#!/usr/bin/env bash
#
# Scans for secrets with the gitleaks CLI, under .gitleaks.toml. The CLI and
# not gitleaks-action, which needs a license on an organization's repos.
#
# The pinned release is downloaded once into .tools/bin and its checksum
# checked against the one written here, so a changed download fails instead
# of running.
#
# Usage:
#   check-secrets.sh                 every tracked file, as staged in the index
#   check-secrets.sh --range A..B    every commit in the range, as a pull request adds them
#   check-secrets.sh --staged        what the next commit adds
#
# Exits 0 when clean, 1 on a finding, 2 when it can't run.
set -euo pipefail

GITLEAKS_VERSION=8.30.1
# From gitleaks_8.30.1_checksums.txt on the release.
gitleaks_sha256() {
  case "$1" in
    linux_x64) echo 551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb ;;
    linux_arm64) echo e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080 ;;
    darwin_x64) echo dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709 ;;
    darwin_arm64) echo b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5 ;;
    *) return 1 ;;
  esac
}

die() { echo "secrets: $*" >&2; exit 2; }

root="$(git rev-parse --show-toplevel)" || die "not in a git repository"
cd "$root"

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{ print $1 }'
}

install_gitleaks() {
  local os arch platform want tmp got
  case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) die "no gitleaks build for $(uname -s)" ;; esac
  case "$(uname -m)" in x86_64 | amd64) arch=x64 ;; arm64 | aarch64) arch=arm64 ;; *) die "no gitleaks build for $(uname -m)" ;; esac
  platform="${os}_${arch}"
  want="$(gitleaks_sha256 "$platform")" || die "no pinned checksum for $platform"
  tmp="$(mktemp -d)"
  curl -fsSL --retry 3 -o "$tmp/gitleaks.tar.gz" \
    "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_${platform}.tar.gz" ||
    die "couldn't download gitleaks $GITLEAKS_VERSION"
  got="$(sha256 "$tmp/gitleaks.tar.gz")"
  [ "$got" = "$want" ] || die "gitleaks_${GITLEAKS_VERSION}_${platform}.tar.gz has checksum $got, want $want"
  tar -xzf "$tmp/gitleaks.tar.gz" -C "$tmp" gitleaks || die "couldn't unpack gitleaks"
  mkdir -p "$(dirname "$bin")"
  mv "$tmp/gitleaks" "$bin"
  rm -rf "$tmp"
}

bin="${TOOLS_BIN:-$root/.tools/bin}/gitleaks-$GITLEAKS_VERSION"
[ -x "$bin" ] || install_gitleaks

# gitleaks exits 1 on a finding. Anything else that isn't 0 is its own
# failure, and reads as one.
run() {
  local rc=0
  "$bin" "$@" --config "$root/.gitleaks.toml" --redact --no-banner --exit-code 1 || rc=$?
  case "$rc" in
    0) echo "secrets: no findings" ;;
    1) echo "secrets: gitleaks found something. If it is a test value, add a narrow allowlist to .gitleaks.toml" >&2 ;;
    *) echo "secrets: gitleaks failed with exit code $rc" >&2; rc=2 ;;
  esac
  return "$rc"
}

case "${1:-}" in
  "")
    # A copy of the index, so untracked files and .tools/ aren't scanned and
    # what's scanned is what would be committed.
    tree="$(mktemp -d)"
    trap 'rm -rf "$tree"' EXIT
    git checkout-index --all --prefix="$tree/" || die "couldn't copy the tracked files"
    run dir "$tree"
    ;;
  --range)
    [ -n "${2:-}" ] || die "--range needs A..B"
    run git --log-opts="$2" .
    ;;
  --staged)
    run git --pre-commit --staged .
    ;;
  *) die "unknown argument $1. See the usage at the top of $0" ;;
esac
