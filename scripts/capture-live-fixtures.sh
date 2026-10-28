#!/usr/bin/env bash
# Capture redacted read-only live fixtures. This never creates resources.
set -euo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=lib/common.sh
. scripts/lib/common.sh

[ "${FIXTURE_CAPTURE_CONFIRM:-}" = "1" ] || {
  echo "Set FIXTURE_CAPTURE_CONFIRM=1 after supplying a human-approved target." >&2
  exit 2
}

endpoint="${K0RDENT_ENDPOINT:?K0RDENT_ENDPOINT is required}"
region="${K0RDENT_REGION:?K0RDENT_REGION is required}"

# The mint below sends the credential without the provider, so this script
# applies the provider's https rule itself.
endpoint_is_encrypted "$endpoint" || {
  echo "K0RDENT_ENDPOINT is $endpoint, and the credential goes in the body of the token request." >&2
  echo "Use https. http is accepted only for localhost or a loopback IP. The provider refuses the same." >&2
  exit 2
}

# Only this user: the fixtures are redacted, but they describe a real landscape.
fixture_dir="${FIXTURE_DIR:-.conformance/fixtures/$(date -u +%Y%m%dT%H%M%SZ)}"
private_dir "$fixture_dir"

if [ -n "${K0RDENT_CLIENT_ID:-}" ] && [ -n "${K0RDENT_CLIENT_SECRET:-}" ]; then
  body="$(token_request_body "$K0RDENT_CLIENT_ID" "$K0RDENT_CLIENT_SECRET" "")"
elif [ -n "${K0RDENT_API_KEY:-}" ]; then
  body="$(token_request_body "" "" "$K0RDENT_API_KEY")"
else
  echo "Set one supported K0RDENT credential source." >&2
  exit 2
fi

token="$(printf '%s' "$body" | curl -fsS --connect-timeout 5 --max-time 30 \
  -X POST "$endpoint/v1/regions/global/auth/token" -H 'Content-Type: application/json' --data-binary @- \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"

# The redactor reads the response on stdin, so its program goes in -c. A
# heredoc on `python3 -` would take stdin and leave json.load nothing to read.
# shellcheck disable=SC2016 # python, not shell
REDACT='
import json, re, sys

secret = re.compile(r"(authorization|credential|secret|token|password|api.?key|kubeconfig)", re.I)
def redact(value, key=""):
    if secret.search(key):
        return "[REDACTED]"
    if isinstance(value, dict):
        return {k: redact(v, k) for k, v in value.items()}
    if isinstance(value, list):
        return [redact(v) for v in value]
    return value
print(json.dumps(redact(json.load(sys.stdin)), indent=2, sort_keys=True))
'

capture() {
  local name="$1" path="$2"
  curl_bearer "$token" -fsS --connect-timeout 5 --max-time 30 "$endpoint$path" |
    python3 -c "$REDACT" > "$fixture_dir/$name.json"
}

capture regions /v1/regions
capture cluster-types "/v1/regions/$region/compute/cluster-types"

# The endpoint and region go in through the environment, so a quote in either
# stays data and never becomes python.
CAP_ENDPOINT="$endpoint" CAP_REGION="$region" python3 -c '
import json, os
print(json.dumps({
  "target": "live",
  "endpoint": os.environ["CAP_ENDPOINT"],
  "region": os.environ["CAP_REGION"],
  "credential_identity": "configured (redacted)",
  "request_bodies": "not captured",
  "redaction": "credential, bearer, and kubeconfig fields removed",
}, indent=2))' > "$fixture_dir/manifest.json"
printf 'Wrote redacted read-only fixtures to %s\n' "$fixture_dir"
