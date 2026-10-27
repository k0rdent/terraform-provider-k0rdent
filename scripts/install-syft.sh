#!/usr/bin/env bash
#
# Fetches the pinned syft release, which goreleaser shells out to for the
# SBOMs, and prints the directory it's in. Each run downloads into a new
# private directory and checks the archive against the checksum written here.
# The release signing job runs it, so what it installs runs beside the key.
#
# Not anchore/sbom-action/download-syft: pinning that action to a commit
# doesn't pin syft, because it runs the install.sh on syft's main branch.
#
# Usage: dir="$(scripts/install-syft.sh)" && PATH="$dir:$PATH" goreleaser ...
# then remove "$dir".
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
want_sha="$(syft_sha256 "$platform")" || die "no pinned checksum for $platform"

syft_dir="$(mktemp -d "${TMPDIR:-/tmp}/syft.XXXXXX")" || die "could not create a directory for syft"
chmod 700 "$syft_dir"
# A failed run leaves nothing behind. A good one leaves the directory to its
# caller, which removes it once goreleaser is done.
trap 'rm -rf "$syft_dir"' EXIT

archive="syft_${SYFT_VERSION}_${platform}.tar.gz"
curl -fsSL --retry 3 -o "$syft_dir/$archive" \
  "https://github.com/anchore/syft/releases/download/v${SYFT_VERSION}/$archive" ||
  die "couldn't download syft $SYFT_VERSION"

got_sha="$(sha256 "$syft_dir/$archive")"
[ "$got_sha" = "$want_sha" ] || die "$archive has checksum $got_sha, want $want_sha"

tar -xzf "$syft_dir/$archive" -C "$syft_dir" syft || die "couldn't unpack syft"
rm -f "$syft_dir/$archive"
trap - EXIT
printf '%s\n' "$syft_dir"
