#!/usr/bin/env bash
set -euo pipefail

tfplugindocs="${1:?usage: generate-provider-docs.sh <tfplugindocs-path>}"
provider_name="${PROVIDER_NAME:?PROVIDER_NAME must be set}"
tf_bin="${TF_BIN:-terraform}"
registry_host="${TF_REGISTRY_HOST:-registry.terraform.io}"
version="0.0.1"
os="$(go env GOOS)"
arch="$(go env GOARCH)"
# A directory only this user can enter: the provider binary built into it is
# run by Terraform.
# shellcheck source=lib/common.sh
. "$(dirname "$0")/lib/common.sh"
tmp="$(private_tmpdir)" || exit 1

cleanup() {
  rm -rf "$tmp"
}
trap cleanup EXIT

if ! command -v "$tf_bin" >/dev/null 2>&1; then
  echo "provider docs: Terraform CLI '$tf_bin' is not on PATH" >&2
  exit 1
fi

plugin_dir="$tmp/plugins/$registry_host/hashicorp/$provider_name/$version/${os}_${arch}"
mkdir -p "$plugin_dir"
go build -o "$plugin_dir/terraform-provider-${provider_name}_v${version}" .

cat > "$tmp/provider.tf" <<EOF
provider "$provider_name" {
}
EOF

(
  cd "$tmp"
  "$tf_bin" init -plugin-dir=./plugins -input=false -no-color >/dev/null
  "$tf_bin" providers schema -json > providers-schema.json
)

sed "s#${registry_host}/hashicorp/#registry.terraform.io/hashicorp/#g" \
  "$tmp/providers-schema.json" > "$tmp/rendered-providers-schema.json"

"$tfplugindocs" generate \
  --provider-name "$provider_name" \
  --rendered-provider-name "$provider_name" \
  --providers-schema "$tmp/rendered-providers-schema.json"

# The Timeouts and Related sections. tfplugindocs has nowhere to put either,
# so they are written into the pages it just generated, from the same schemas.
go run ./internal/tools/docsections -docs docs
