#!/usr/bin/env bash
set -euo pipefail

dist_dir="${1:-dist}"
provider="terraform-provider-k0rdent"
expected_version="${EXPECTED_VERSION:-}"
require_signature="${REQUIRE_SIGNATURE:-true}"

if [[ ! -d "$dist_dir" ]]; then
  echo "release artifact validation: $dist_dir does not exist" >&2
  exit 1
fi

if [[ "$require_signature" != true && "$require_signature" != false ]]; then
  echo "release artifact validation: REQUIRE_SIGNATURE must be true or false" >&2
  exit 1
fi

mapfile -t archives < <(find "$dist_dir" -maxdepth 1 -type f -name "${provider}_*.zip" -printf '%f\n' | sort)
expected_platforms=(
  "darwin_amd64"
  "darwin_arm64"
  "linux_amd64"
  "linux_arm64"
  "windows_amd64"
)

if [[ "${#archives[@]}" -ne "${#expected_platforms[@]}" ]]; then
  echo "release artifact validation: expected ${#expected_platforms[@]} provider archives, found ${#archives[@]}" >&2
  printf '  %s\n' "${archives[@]:-}" >&2
  exit 1
fi

release_version=""
expected_archives=()

for platform in "${expected_platforms[@]}"; do
  matches=("$dist_dir"/"${provider}"_*_"${platform}".zip)
  if [[ ! -e "${matches[0]}" || "${#matches[@]}" -ne 1 ]]; then
    echo "release artifact validation: expected one archive for $platform" >&2
    exit 1
  fi

  archive="${matches[0]}"
  archive_name="$(basename "$archive")"
  version="${archive_name#"${provider}_"}"
  version="${version%"_${platform}.zip"}"

  if [[ -z "$version" || "$version" == "$archive_name" ]]; then
    echo "release artifact validation: could not determine version from $archive_name" >&2
    exit 1
  fi

  if [[ -z "$release_version" ]]; then
    release_version="$version"
  elif [[ "$version" != "$release_version" ]]; then
    echo "release artifact validation: $archive_name has version $version, expected $release_version" >&2
    exit 1
  fi

  expected_archives+=("$archive_name")

  # Each SBOM is in the signed checksums with its archive. One outside them
  # reads as provenance and carries none.
  sbom_name="${archive_name}.sbom.json"
  if [[ ! -f "$dist_dir/$sbom_name" ]]; then
    echo "release artifact validation: missing $sbom_name" >&2
    exit 1
  fi
  expected_archives+=("$sbom_name")

  binary="${provider}_v${version}"
  if [[ "$platform" == windows_* ]]; then
    binary="${binary}.exe"
  fi

  mapfile -t binaries < <(unzip -Z1 "$archive" | grep -E "^${provider}_v.+(\\.exe)?$")
  if [[ "${#binaries[@]}" -ne 1 || "${binaries[0]}" != "$binary" ]]; then
    echo "release artifact validation: $(basename "$archive") does not contain $binary" >&2
    exit 1
  fi
done

if [[ -n "$expected_version" && "$release_version" != "$expected_version" ]]; then
  echo "release artifact validation: archives have version $release_version, expected $expected_version" >&2
  exit 1
fi

manifest_name="${provider}_${release_version}_manifest.json"
if ! cmp -s "$dist_dir/$manifest_name" terraform-registry-manifest.json; then
  echo "release artifact validation: $manifest_name is missing or differs from terraform-registry-manifest.json" >&2
  exit 1
fi
expected_archives+=("$manifest_name")

checksum_name="${provider}_${release_version}_SHA256SUMS"
mapfile -t checksums < <(find "$dist_dir" -maxdepth 1 -type f -name "${provider}_*_SHA256SUMS" -printf '%f\n' | sort)
if [[ "${#checksums[@]}" -ne 1 || "${checksums[0]:-}" != "$checksum_name" ]]; then
  echo "release artifact validation: expected only $checksum_name" >&2
  exit 1
fi

checksum_path="$dist_dir/$checksum_name"
(
  cd "$dist_dir"
  sha256sum --check "$checksum_name"
)

mapfile -t checksum_archives < <(
  awk 'NF == 2 {sub(/^\*/, "", $2); print $2}' "$checksum_path" | sort
)
mapfile -t expected_archives < <(printf '%s\n' "${expected_archives[@]}" | sort)

if [[ "${#checksum_archives[@]}" -ne "${#expected_archives[@]}" ]] || \
  ! diff -u <(printf '%s\n' "${expected_archives[@]}") <(printf '%s\n' "${checksum_archives[@]}"); then
  echo "release artifact validation: $checksum_name must cover exactly the five release archives, their five SBOMs and the registry manifest" >&2
  exit 1
fi

signature_path="${checksum_path}.sig"
if [[ "$require_signature" == true ]]; then
  if [[ ! -f "$signature_path" ]]; then
    echo "release artifact validation: missing detached signature $(basename "$signature_path")" >&2
    exit 1
  fi
  # A bare --verify passes for any key in the keyring, so the VALIDSIG status
  # line is matched against the release key: its first field is the signing
  # subkey, its last the primary key.
  if [[ -n "${GPG_KEY_FINGERPRINT:-}" ]]; then
    # VALIDSIG is 40 uppercase hex digits, with no spaces.
    fingerprint="$(printf '%s' "$GPG_KEY_FINGERPRINT" | tr -d ' ' | tr '[:lower:]' '[:upper:]')"
    if ! gpg --batch --status-fd=1 --verify "$signature_path" "$checksum_path" 2>/dev/null |
      grep -qE "^\[GNUPG:\] VALIDSIG (${fingerprint} .*|.* ${fingerprint})$"; then
      echo "release artifact validation: $(basename "$signature_path") does not carry a valid signature from ${fingerprint}" >&2
      exit 1
    fi
  else
    echo "release artifact validation: GPG_KEY_FINGERPRINT is not set, so the signature cannot be bound to the release key" >&2
    exit 1
  fi
fi

echo "release artifact validation: verified ${#archives[@]} archives, $checksum_name, and signature requirement=$require_signature"
