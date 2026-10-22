#!/usr/bin/env bash
#
# Copies terraform-registry-manifest.json into dist/ under the name the
# registry expects, beside the checksums that already list it.
set -euo pipefail

dist_dir="${1:-dist}"
shopt -s nullglob
sums=("$dist_dir"/*_SHA256SUMS)
if [[ "${#sums[@]}" -ne 1 ]]; then
  echo "place-manifest: expected one *_SHA256SUMS in $dist_dir, found ${#sums[@]}" >&2
  exit 1
fi
cp terraform-registry-manifest.json "${sums[0]%_SHA256SUMS}_manifest.json"
