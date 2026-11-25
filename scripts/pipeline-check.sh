#!/usr/bin/env bash
set -euo pipefail
umask 077
REPO=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO"
# shellcheck source=lib/common.sh
. "$REPO/scripts/lib/common.sh"

mode=${PIPELINE_MODE:-fake}
case "$mode" in
  fake) ;;
  live)
    [ -n "${K0RDENT_ENDPOINT:-}" ] && endpoint_is_encrypted "$K0RDENT_ENDPOINT" || die 'Set an HTTPS or loopback K0RDENT_ENDPOINT.'
    [ "${PIPELINE_CONFIRM:-}" = "$(endpoint_host "$K0RDENT_ENDPOINT")" ] || die 'Set PIPELINE_CONFIRM to the endpoint host and port before creating live clusters.'
    : "${PIPELINE_PREFIX:?Set a prefix for cluster ids on the live endpoint.}"
    : "${K0RDENT_REGION:?Set K0RDENT_REGION.}" "${K0RDENT_PROJECT:?Set K0RDENT_PROJECT.}" "${REAL_API_CLUSTER_TYPE:?Set REAL_API_CLUSTER_TYPE.}"
    ;;
  *) die 'PIPELINE_MODE must be fake or live.' ;;
esac

TF_BIN=${TF_BIN:-terraform}
need "$TF_BIN"
need python3
version=$(CHECKPOINT_DISABLE=1 "$TF_BIN" version)
[[ "${version%%$'\n'*}" == 'Terraform v1.15.5' ]] || die 'Use HashiCorp Terraform 1.15.5 (set TF_BIN to its path).'

work=$(mktemp -d "${TMPDIR:-/tmp}/pipeline.XXXXXX")
printf 'Pipeline artifacts: %s\n' "$work"
read_names
build_provider_mirror "$work" 0.0.1
pids=()
cleanup() {
  local pid
  for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ "$mode" = fake ]; then
  go build -o "$work/fakeapi" ./internal/tools/fakeapi
  start_loopback_server "$work/fakeapi" -secrets-file "$work/fake-secrets.env" "$work/fake.url" "$work/fake.log"
  pids+=("$SERVER_PID")
  export_fake_credential "$work" "$work/fake-secrets.env"
  export K0RDENT_ENDPOINT="$SERVER_URL" K0RDENT_REGION=us-texas-1 K0RDENT_PROJECT=demo REAL_API_CLUSTER_TYPE=gpu-training
  unset K0RDENT_API_KEY
fi

go build -o "$work/statebackend" ./internal/tools/statebackend
start_loopback_server "$work/statebackend" -password-file "$work/backend.env" "$work/backend.url" "$work/backend.log"
pids+=("$SERVER_PID")
# shellcheck source=/dev/null
. "$work/backend.env"
export TF_HTTP_PASSWORD="$STATE_BACKEND_PASSWORD" TF_CLI_CONFIG_FILE="$work/terraform.rc"
export TF_IN_AUTOMATION=1 TF_INPUT=0 CHECKPOINT_DISABLE=1
python3 -B "$REPO/scripts/lib/pipeline_check.py" "$mode" "$work" "$(command -v "$TF_BIN")" "$@"
