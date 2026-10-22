#!/usr/bin/env bash
#
# The configuration-layer harness: what the provider exposes, and how it behaves
# when the credential is missing or wrong.
#
# This is the narrow companion to `scripts/demo.sh`. The demo drives the resource
# lifecycle end to end and is the thing to run to see the provider work. This
# script covers the three things the demo cannot, because the demo only ever runs
# with a good credential:
#
#   * the shipped binary's schema, read over the real plugin protocol — every
#     attribute optional, none sensitive, no credential, no organization, and
#     every resource and data source name derived from the one name constant;
#   * a missing credential produces a diagnostic that says what to set;
#   * a rejected mint reaches the user as a 401 and is NOT retried. That last one
#     matters: a revoked credential answers 401 too, and replaying spends the ten
#     mints a credential is allowed per minute without ever recovering.
#
# Requires: go, python3, and `terraform` or `tofu` on PATH.
#
# WHAT A GREEN RUN PROVES. Everything on the provider's side of the wire is real.
# The control plane is not. See the header of internal/tools/fakeapi/main.go for
# the behaviour it assumes and why those assumptions are deliberately not
# flattering.

set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"
# shellcheck source=../scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"

read_names
find_terraform

WORK="$(private_tmpdir)" || exit 1
FAKE_PID=""
cleanup() {
  [ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# This harness's own heading, in place of the library's.
step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

VERSION=0.0.0-verify
build_provider "$WORK" "$VERSION"
export TF_CLI_CONFIG_FILE="$WORK/terraform.rc"

cat > "$WORK/main.tf" <<EOF
terraform {
  required_providers {
    $PROVIDER_NAME = { source = "$NAMESPACE/$PROVIDER_NAME" }
  }
}

provider "$PROVIDER_NAME" {
  endpoint = var.endpoint
  region   = "us-texas-1"
  project  = "demo"
}

variable "endpoint" { type = string }

data "${PROVIDER_NAME}_cluster_types" "catalog" {}

output "catalog_size" { value = length(data.${PROVIDER_NAME}_cluster_types.catalog.cluster_types) }
EOF

# ---------------------------------------------------------------------------
step "1. What the shipped binary exposes"
# ---------------------------------------------------------------------------
# Read over the real plugin protocol rather than asserted in Go, because this is
# the artifact a user would install.

( cd "$WORK" && "$TF_BIN" providers schema -json ) > "$WORK/schema.json"

PROVIDER_NAME="$PROVIDER_NAME" python3 - "$WORK/schema.json" <<'PY'
import json, os, sys

name = os.environ["PROVIDER_NAME"]
schema = json.load(open(sys.argv[1]))
providers = schema["provider_schemas"]
assert len(providers) == 1, providers.keys()
block = next(iter(providers.values()))

attributes = block["provider"]["block"]["attributes"]
expected = {"endpoint", "region", "project", "operation_timeout", "poll_interval", "default_tags"}
assert set(attributes) == expected, sorted(attributes)

for attr_name, attr in attributes.items():
    assert attr.get("optional"), f"{attr_name} is not optional"
    assert not attr.get("sensitive"), f"{attr_name} is marked sensitive, so it looks like a secret"
    assert attr.get("description"), f"{attr_name} has no description"

# The two absences, checked against what a real Terraform saw rather than against
# the Go source.
for forbidden in ("organization", "org", "tenant",
                  "token", "api_key", "client_id", "client_secret",
                  "credentials", "credentials_file"):
    assert forbidden not in attributes, f"the provider exposes {forbidden}"

# The naming guard, at the level that matters: what Terraform was told.
resources = block.get("resource_schemas", {})
data_sources = block.get("data_source_schemas", {})
assert resources, "the provider registers no resources"
assert data_sources, "the provider registers no data sources"
for kind, entries in (("resource", resources), ("data source", data_sources)):
    for entry in entries:
        assert entry.startswith(name + "_"), f"{kind} {entry} does not start with {name}_"

# A credential must not be settable on a resource either, and a secret the
# platform hands back must be marked sensitive. A computed client_id is the
# public half of an issued credential (a service account names its own), so
# it's allowed as long as nothing can set it.
for kind, entries in (("resource", resources), ("data source", data_sources)):
    for entry, body in entries.items():
        attrs = body["block"]["attributes"]
        for forbidden in ("token", "api_key", "client_id", "client_secret", "credentials"):
            attr = attrs.get(forbidden)
            if attr is None:
                continue
            assert not (attr.get("optional") or attr.get("required")), \
                f"{kind} {entry} lets a configuration set {forbidden}"
            if forbidden != "client_id":
                assert attr.get("sensitive"), f"{kind} {entry} returns {forbidden} unmarked"

print(f"  ok: {len(attributes)} optional provider attributes, none sensitive, "
      "no organization, no credential")
print(f"  ok: {len(resources)} resource(s) and {len(data_sources)} data source(s), "
      f"every name prefixed {name}_")
PY

# ---------------------------------------------------------------------------
step "2. Start the fake control plane"
# ---------------------------------------------------------------------------

go build -o "$WORK/fakeapi" ./internal/tools/fakeapi
# The fake makes its credentials for this run and writes them, 0600, into
# $WORK, which only this user can enter.
start_loopback_server "$WORK/fakeapi" -secrets-file "$WORK/fake-secrets.env" "$WORK/fake.url" "$WORK/fake.log" \
  || { echo "the fake did not start"; cat "$WORK/fake.log"; exit 1; }
FAKE_PID="$SERVER_PID"
ENDPOINT="$SERVER_URL"
# shellcheck source=/dev/null # written by internal/tools/fakeapi
. "$WORK/fake-secrets.env"
echo "  listening on $ENDPOINT"

# ---------------------------------------------------------------------------
step "3. Plan with no credential in the environment"
# ---------------------------------------------------------------------------
# A pipeline with a missing secret is the most common first failure, so the
# diagnostic has to name what to set.

set +e
( cd "$WORK" && env -u "${ENV_PREFIX}CLIENT_ID" -u "${ENV_PREFIX}CLIENT_SECRET" -u "${ENV_PREFIX}API_KEY" \
    "$TF_BIN" plan -no-color -var endpoint="$ENDPOINT" ) > "$WORK/nocred.txt" 2>&1
NOCRED_STATUS=$?
set -e
[ "$NOCRED_STATUS" -ne 0 ] || { echo "planning with no credential succeeded"; cat "$WORK/nocred.txt"; exit 1; }
grep -q "No credential" "$WORK/nocred.txt" || { cat "$WORK/nocred.txt"; exit 1; }
sed -n '/Error: No credential/,/^╵/p' "$WORK/nocred.txt" | sed 's/^/  /'

# ---------------------------------------------------------------------------
step "4. Plan with a bad secret"
# ---------------------------------------------------------------------------

set +e
( cd "$WORK" \
  && export "${ENV_PREFIX}CLIENT_ID=$FAKE_CLIENT_ID" \
  && export "${ENV_PREFIX}CLIENT_SECRET=k0r_wrong" \
  && "$TF_BIN" plan -no-color -var endpoint="$ENDPOINT" ) > "$WORK/badcred.txt" 2>&1
BADCRED_STATUS=$?
set -e
[ "$BADCRED_STATUS" -ne 0 ] || { echo "planning with a bad secret succeeded"; cat "$WORK/badcred.txt"; exit 1; }
grep -qi '401\|unauthorized' "$WORK/badcred.txt" || { cat "$WORK/badcred.txt"; exit 1; }
grep -Ei 'unauthorized|401' "$WORK/badcred.txt" | head -3 | sed 's/^/  /'

MINTS=$(grep -c 'POST /v1/regions/global/auth/token' "$WORK/fake.log")
[ "$MINTS" -eq 1 ] || { echo "a rejected mint was retried $MINTS times"; exit 1; }
echo "  ok: one mint attempt, not retried"

# ---------------------------------------------------------------------------
step "5. Plan with a good service account credential"
# ---------------------------------------------------------------------------
# The whole path: HCL -> plugin protocol -> Configure -> environment ->
# generated client -> anonymous mint -> bearer token -> authorized regional read.

( cd "$WORK" \
  && export "${ENV_PREFIX}CLIENT_ID=$FAKE_CLIENT_ID" \
  && export "${ENV_PREFIX}CLIENT_SECRET=$FAKE_CLIENT_SECRET" \
  && "$TF_BIN" plan -no-color -var endpoint="$ENDPOINT" ) > "$WORK/plan.txt" 2>&1 \
  || { cat "$WORK/plan.txt"; exit 1; }

grep -E 'No changes|catalog_size' "$WORK/plan.txt" | head -4 | sed 's/^/  /'

# ---------------------------------------------------------------------------
step "6. What the fake saw"
# ---------------------------------------------------------------------------

grep -E 'POST /v1/regions/global/auth/token|GET /v1/regions/' "$WORK/fake.log" | tail -4 | sed 's/^/  /'

grep -q 'GET /v1/regions/us-texas-1/compute/cluster-types .* auth=present' "$WORK/fake.log" \
  || { echo "the regional read did not carry the minted token"; cat "$WORK/fake.log"; exit 1; }
echo "  ok: the regional read carried the token minted on the global tier"

# The provider's product token comes after Terraform's own, as HashiCorp's
# SDK writes the header, so it's matched anywhere inside the quoted value.
grep -q "ua=\"[^\"]*terraform-provider-${PROVIDER_NAME}/${VERSION}[^\"]*\"" "$WORK/fake.log" \
  || { echo "no versioned provider user agent reached the server"; exit 1; }
echo "  ok: requests identify the provider and its version"

# ---------------------------------------------------------------------------
step "7. What the fake refuses"
# ---------------------------------------------------------------------------
# Loopback is reachable by every account on the machine and by any browser
# page, so the fake serves only this run. Each of these is a request one of
# them could make, and none of them carries this run's secret or Host.

mint_status() { # <client secret> [curl args...]
  local secret="$1"; shift
  token_request_body "$FAKE_CLIENT_ID" "$secret" "" \
    | curl -sS -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
        --data-binary @- "$@" "$ENDPOINT/v1/regions/global/auth/token"
}
[ "$(mint_status k0r_fake_secret_for_local_verification_only)" = 401 ] \
  || { echo "the fake minted with the secret this repository used to publish"; exit 1; }
echo "  ok: the secret this repository used to publish no longer mints"
[ "$(mint_status "$FAKE_CLIENT_SECRET" -H "Host: rebound.example:${ENDPOINT##*:}")" = 403 ] \
  || { echo "the fake served a Host that isn't its own"; exit 1; }
echo "  ok: a request under another host name, as a rebinding page makes, is refused"
[ "$(mint_status "$FAKE_CLIENT_SECRET" -H 'Origin: http://attacker.example')" = 403 ] \
  || { echo "the fake served a browser's request"; exit 1; }
echo "  ok: a request a browser made is refused"
[ "$(mint_status "$FAKE_CLIENT_SECRET")" = 200 ] || { echo "this run's own secret did not mint"; exit 1; }
echo "  ok: this run's secret mints"

printf '\n\033[1mPASS\033[0m — against a fake. Run scripts/demo.sh for the resource lifecycle.\n'
