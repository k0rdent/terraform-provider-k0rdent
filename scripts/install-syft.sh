#!/usr/bin/env bash
#
# Fetches the pinned syft release, which goreleaser shells out to for the
# SBOMs, and prints the directory it's in. Each run downloads into a new
# private directory and checks the archive against the checksum written here,
# so a changed download fails instead of running, and nothing left over from
# an earlier run is used. The release signing job runs it, so what it installs
# runs beside the signing key.
#
# anchore/sbom-action/download-syft did this before. Pinning that action to a
# commit didn't pin syft: it runs the install.sh on syft's main branch.
#
# Usage: PATH="$(scripts/install-syft.sh):$PATH" goreleaser ...
set -euo pipefail

SYFT_VERSION=1.42.3
# From syft_1.42.3_checksums.txt on the release.
syft_sha256() {
  case "$1" in
    linux_amd64) echo 0d6be741479eddd2c8644a288990c04f3df0d609bbc1599a005532a9dff63509 ;;
    linux_arm64) echo dc630590c953347789d08f8ebf57c7d8094db89100785fcd94b1cddeac791804 ;;
    darwin_amd64) echo c00d01b7c43504708c8922d643d1cbaefa62ca8876baa4d2c2cf4c3be43707a9 ;;
    darwin_arm64) echo d71ee7db2be0fe2e96f679fd9d69ef04274cc86c8604707797080a21070b3f32 ;;
    *) return 1 ;;
  esac
}

die() { echo "install-syft: $*" >&2; exit 1; }

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{ print $1 }'
}

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) die "no syft build for $(uname -s)" ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; arm64 | aarch64) arch=arm64 ;; *) die "no syft build for $(uname -m)" ;; esac
platform="${os}_${arch}"
want="$(syft_sha256 "$platform")" || die "no pinned checksum for $platform"

dir="$(mktemp -d)" || die "could not create a directory for syft"
chmod 700 "$dir"
archive="syft_${SYFT_VERSION}_${platform}.tar.gz"
curl -fsSL --retry 3 -o "$dir/$archive" \
  "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/$archive" ||
  die "couldn't download syft $SYFT_VERSION"
got="$(sha256 "$dir/$archive")"
[ "$got" = "$want" ] || die "$archive has checksum $got, want $want"
tar -xzf "$dir/$archive" -C "$dir" syft || die "couldn't unpack syft"
rm -f "$dir/$archive"
printf '%s\n' "$dir"
