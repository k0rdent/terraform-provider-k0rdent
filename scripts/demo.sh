#!/usr/bin/env bash
#
# The demo. One command that shows the whole provider working, on a laptop,
# against a local fake control plane.
#
#   scripts/demo.sh            the full sequence, then clean up
#   scripts/demo.sh setup      start the fake and build the provider
#   scripts/demo.sh session    run an interactive shell that owns the fake
#   scripts/demo.sh cleanup    stop only the fake owned by this run directory
#   scripts/demo.sh env        print the exports setup created, for a new shell
#
# NEEDS NOTHING SECRET. No API credential, no GitHub token, no registry account,
# no access to the k0rdent org, and no network except to a fake on 127.0.0.1.
#
# PREREQUISITES
#   * Go 1.26.10 or newer. go.mod declares `go 1.26.10`, and a lower
#     toolchain fails outright rather than degrading.
#   * `terraform` or `tofu` on PATH. On macOS: `brew install terraform`, or
#     `brew install opentofu`. The script prefers terraform and falls back.
#   * A populated Go module cache, or network access on the first run.
#
# WHAT THIS PROVES, AND WHAT IT DOES NOT
#   Everything on the provider's side of the wire is real: real HCL, real plugin
#   protocol, real provider binary, real generated client, real token mint over
#   HTTP. The control plane is not. A green run says the provider's wiring is
#   right *given the behaviour the fake models* — and the fake deliberately does
#   not echo back what it was sent. Read the ASSUMPTION comments in
#   internal/tools/fakeapi/main.go for what it assumes and why.
#
#   It says nothing about the real server. Nobody has run this against a live
#   landscape; there is no credential for one yet.

set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"
DEMO_DIR="$REPO/examples/demo"
RUN_DIR="${DEMO_RUN_DIR:-$REPO/.demo}"
case "$RUN_DIR" in /*) ;; *) RUN_DIR="$REPO/$RUN_DIR" ;; esac
DEMO_TF_DIR="${DEMO_TF_DIR:-$RUN_DIR/terraform}"
case "$DEMO_TF_DIR" in /*) ;; *) DEMO_TF_DIR="$REPO/$DEMO_TF_DIR" ;; esac
PORT_FILE="$RUN_DIR/endpoint"
# The fake makes its credentials for each run and writes them here, 0600. Its
# owner token, in the same file, is what tells this script the fake on the
# port is the one it started.
SECRETS_FILE="$RUN_DIR/fake-secrets.env"
LOG_FILE="$RUN_DIR/fake.log"

# shellcheck source=lib/common.sh
. "$REPO/scripts/lib/common.sh"

# ---------------------------------------------------------------------------
# The fake
# ---------------------------------------------------------------------------

# owner_token prints the owner token from this run's secrets file.
owner_token() {
  # shellcheck source=/dev/null # written by internal/tools/fakeapi
  ( . "$SECRETS_FILE" && printf '%s' "$FAKE_OWNER_TOKEN" )
}

fake_running() {
  [ -s "$PORT_FILE" ] && [ -s "$SECRETS_FILE" ] || return 1
  local endpoint token status
  endpoint="$(cat "$PORT_FILE")"
  token="$(owner_token)"
  status="$(curl_bearer "$token" -sS -A k0rdent-test-harness -o /dev/null -w '%{http_code}' \
    --connect-timeout 1 --max-time 2 "$endpoint/_harness/ready" 2>/dev/null || true)"
  [ "$status" = "204" ]
}

start_fake() {
  private_dir "$RUN_DIR"
  if fake_running; then
    ENDPOINT="$(cat "$PORT_FILE")"
    good "reusing the fake already listening on $ENDPOINT"
    return
  fi

  go build -o "$RUN_DIR/fakeapi" ./internal/tools/fakeapi
  if start_loopback_server "$RUN_DIR/fakeapi" -secrets-file "$SECRETS_FILE" "$PORT_FILE" "$LOG_FILE"; then
    ENDPOINT="$SERVER_URL"
    local _
    for _ in $(seq 1 100); do
      if fake_running; then
        good "fake control plane listening on $ENDPOINT"
        note "request log: $LOG_FILE"
        return
      fi
      sleep 0.1
    done
  fi
  cat "$LOG_FILE" >&2
  die "the fake control plane did not become ready; no provider step was run"
}

stop_fake() {
  if [ ! -s "$PORT_FILE" ] || [ ! -s "$SECRETS_FILE" ]; then
    rm -f "$PORT_FILE" "$SECRETS_FILE"
    note "no owned fake was running"
    return
  fi

  local endpoint token status
  endpoint="$(cat "$PORT_FILE")"
  token="$(owner_token)"
  status="$(curl_bearer "$token" -sS -A k0rdent-test-harness -o /dev/null -w '%{http_code}' \
    --connect-timeout 1 --max-time 2 "$endpoint/_harness/ready" 2>/dev/null || true)"
  case "$status" in
    204)
      status="$(curl_bearer "$token" -sS -A k0rdent-test-harness -o /dev/null -w '%{http_code}' \
        --connect-timeout 1 --max-time 2 -X POST "$endpoint/_harness/shutdown" 2>/dev/null || true)"
      [ "$status" = "202" ] || die "the fake rejected owner-token shutdown (HTTP $status); preserving run files"
      local _
      for _ in $(seq 1 100); do
        if [ "$(curl_bearer "$token" -sS -A k0rdent-test-harness -o /dev/null -w '%{http_code}' \
          --connect-timeout 1 --max-time 1 "$endpoint/_harness/ready" 2>/dev/null || true)" = "000" ]; then
          break
        fi
        sleep 0.1
      done
      good "stopped the owned fake control plane"
      ;;
    000) note "the owned fake is already stopped" ;;
    *) die "owner-token readiness check failed (HTTP $status); preserving run files" ;;
  esac
  rm -f "$PORT_FILE" "$SECRETS_FILE"
}

seed_demo_dir() {
  mkdir -p "$DEMO_TF_DIR"
  if [ ! -f "$DEMO_TF_DIR/main.tf" ]; then
    cp -R "$DEMO_DIR/." "$DEMO_TF_DIR/"
  fi
}

# print_env prints what a new shell needs. The credential is this fake's, made
# for this run, and every value in the secrets file is a fixed prefix and hex,
# so single quotes are enough.
print_env() {
  # shellcheck source=/dev/null # written by internal/tools/fakeapi
  . "$SECRETS_FILE"
  cat <<EOF
# Paste these into a shell to drive the demo directory by hand.
export DEMO_RUN_DIR="$RUN_DIR"
export TF_CLI_CONFIG_FILE="$RUN_DIR/terraform.rc"
export ${ENV_PREFIX}CLIENT_ID='$FAKE_CLIENT_ID'
export ${ENV_PREFIX}CLIENT_SECRET='$FAKE_CLIENT_SECRET'
export TF_VAR_endpoint='$ENDPOINT'
cd "$DEMO_TF_DIR"
$TF_BIN plan -var endpoint="$ENDPOINT"
EOF
}

tf() { ( cd "$DEMO_TF_DIR" && "$TF_BIN" "$@" ); }

# ---------------------------------------------------------------------------
# Subcommands
# ---------------------------------------------------------------------------

setup_environment() {
  read_names; find_terraform
  start_fake
  build_provider "$RUN_DIR" 0.0.0-demo
  seed_demo_dir
  export_fake_credential "$RUN_DIR" "$SECRETS_FILE"
}

cmd_setup() {
  setup_environment
  printf '\n%sThe fake is running and the provider is built.%s\n\n' "$B" "$R"
  print_env
  printf '\nStop it with: %sDEMO_RUN_DIR=%q scripts/demo.sh cleanup%s\n\n' "$B" "$RUN_DIR" "$R"
}

cmd_cleanup() { stop_fake; }

cmd_env() {
  read_names; find_terraform
  fake_running || die "no fake is ready for this run directory; start it with scripts/demo.sh setup"
  ENDPOINT="$(cat "$PORT_FILE")"
  print_env
}

# Some agent command runners terminate every background child after a command
# exits. Keep the fake, provider environment, and user's shell in one managed
# process tree; leaving the shell runs the ownership-aware cleanup trap.
cmd_session() {
  private_dir "$RUN_DIR"
  export GOCACHE="${GOCACHE:-$RUN_DIR/go-build}"
  export GOMODCACHE="${GOMODCACHE:-$RUN_DIR/go/pkg/mod}"
  mkdir -p "$GOCACHE" "$GOMODCACHE"
  read_names; find_terraform
  trap stop_fake EXIT
  start_fake
  build_provider "$RUN_DIR" 0.0.0-demo
  export_fake_credential "$RUN_DIR" "$SECRETS_FILE"
  printf '\nManaged demo session ready at %s. Type exit to stop its fake.\n' "$ENDPOINT"
  printf 'Run Terraform from a private work directory with -var endpoint=%s.\n\n' "$ENDPOINT"
  bash --noprofile --norc -i
}

cmd_run() {
  read_names; find_terraform

  # Not `local`: the EXIT trap runs after this function returns.
  keep="${DEMO_KEEP:-0}"
  cleanup() {
    if [ "${keep:-0}" = "1" ]; then
      printf '\n%sThe fake is still running on %s.%s Stop it with: %sscripts/demo.sh down%s\n\n' \
        "$DIM" "$ENDPOINT" "$R" "$B" "$R"
    else
      stop_fake
    fi
  }
  trap cleanup EXIT

  printf '%sk0rdent AI Terraform provider — local demo%s\n' "$B" "$R"
  note "runner: $($TF_BIN version | head -1),  $(go version | awk '{print $3}')"
  note "no credential, no registry, no network beyond 127.0.0.1"

  # -------------------------------------------------------------------------
  step "1/8  Build the provider and start the fake control plane"
  # -------------------------------------------------------------------------
  start_fake
  build_provider "$RUN_DIR" 0.0.0-demo
  seed_demo_dir
  export_fake_credential "$RUN_DIR" "$SECRETS_FILE"
  note "the API credential is the fake's own, made for this run and exported into this shell only"

  # -------------------------------------------------------------------------
  step "2/8  No init — and why, because this is the confusing part"
  # -------------------------------------------------------------------------
  rm -rf "$DEMO_TF_DIR/.terraform" "$DEMO_TF_DIR/.terraform.lock.hcl" \
         "$DEMO_TF_DIR/terraform.tfstate" "$DEMO_TF_DIR/terraform.tfstate.backup"
  warn "This demo does not run \`$TF_BIN init\`, on purpose."
  warn "Under dev_overrides the tool runs the binary from step 1 directly, with no"
  warn "registry, no download and no lock file — so there is nothing to initialise."
  warn "Running init anyway tries to resolve the provider from the public registry,"
  warn "where nothing has been published, and fails. The tool says so itself:"
  warn "  \"Skip $TF_BIN init when using provider development overrides.\""
  warn ""
  warn "Every command below prints a yellow \"Provider development overrides are in"
  warn "effect\" warning. That is the override working, not a failure."
  good "nothing to initialise; skipping init is correct here"

  # -------------------------------------------------------------------------
  step "3/8  terraform apply — create the cluster and the instance group"
  # -------------------------------------------------------------------------
  tf apply -auto-approve -input=false -no-color -var endpoint="$ENDPOINT" \
    || die "terraform apply failed"
  good "applied"

  # -------------------------------------------------------------------------
  step "4/8  terraform plan — THE MEASUREMENT"
  # -------------------------------------------------------------------------
  printf '%s   Everything else here is setup. This is the claim: a plan straight after an\n' "$DIM"
  printf '   apply, with the configuration untouched, must report no changes. Exit code 0\n'
  printf '   means no changes, 2 means changes, 1 means error.%s\n' "$R"
  set +e
  tf plan -detailed-exitcode -input=false -no-color -var endpoint="$ENDPOINT"
  local plan_status=$?
  set -e
  case "$plan_status" in
    0) good "exit 0 — no changes. The round trip held." ;;
    2) die "exit 2 — the plan proposes changes after an apply that changed nothing.
   That is a real finding. Read the diff above: if the provider is wrong, fix the
   provider; if the contract is wrong, file it as a spec finding. Do not paper
   over it." ;;
    *) die "terraform plan failed with exit $plan_status" ;;
  esac
  printf '\n%s   What that does and does not mean: the fake does not echo back what it was\n' "$DIM"
  printf '   sent. It resolves the unversioned cluster type to a pinned version, returns\n'
  printf '   relative paths for a field declared format:uri, materializes node pools the\n'
  printf '   configuration never mentioned, drops an empty tag map, and answers an empty\n'
  printf '   list by omitting the key. So exit 0 is a bounded claim — correct given those\n'
  printf '   assumptions — not a tautology. None of them has been checked against a real\n'
  printf '   server.%s\n' "$R"

  # -------------------------------------------------------------------------
  step "5/8  The round trip, made visible"
  # -------------------------------------------------------------------------
  tf output -no-color cluster_type_round_trip
  note "different strings, no diff: the provider tolerates the platform's resolution"
  note "and records it, rather than proposing to change an immutable field forever."

  # -------------------------------------------------------------------------
  step "6/8  Scale the cluster, then plan again"
  # -------------------------------------------------------------------------
  local scaled=$(( ${DEMO_WORKERS:-2} + 2 ))
  note "worker pool 2 -> $scaled nodes"
  tf apply -auto-approve -input=false -no-color \
    -var endpoint="$ENDPOINT" -var worker_nodes="$scaled" || die "the scale apply failed"
  set +e
  tf plan -detailed-exitcode -input=false -no-color \
    -var endpoint="$ENDPOINT" -var worker_nodes="$scaled" >/dev/null
  plan_status=$?
  set -e
  [ "$plan_status" = "0" ] || die "the plan after scaling proposes changes (exit $plan_status)"
  good "scaled, and the plan is still clean"
  tf output -no-color cluster

  # -------------------------------------------------------------------------
  step "7/8  Import — drop the cluster from state and adopt it back"
  # -------------------------------------------------------------------------
  tf state rm -no-color "${PROVIDER_NAME}_kubernetes_cluster.demo" >/dev/null \
    || die "could not remove the cluster from state"
  note "removed from state; the cluster still exists on the platform"
  tf import -input=false -no-color \
    -var endpoint="$ENDPOINT" -var worker_nodes="$scaled" \
    "${PROVIDER_NAME}_kubernetes_cluster.demo" "${DEMO_CLUSTER:-tf-demo}" >/dev/null \
    || die "terraform import failed"
  good "imported — state rebuilt from the platform alone, with no configuration to copy from"

  # An import has no configuration to reconcile against, so it adopts every node
  # pool the platform materialized. This configuration manages one of them. The
  # first plan after import therefore narrows the managed set, and that is
  # correct rather than a defect — it is the ordinary "import, then plan, then
  # make the configuration say what you want" loop. Showing it is more useful
  # than hiding it: the apply that follows makes no API call at all, because
  # every count already matches.
  set +e
  tf plan -detailed-exitcode -input=false -no-color \
    -var endpoint="$ENDPOINT" -var worker_nodes="$scaled" > "$RUN_DIR/import-plan.txt" 2>&1
  plan_status=$?
  set -e
  case "$plan_status" in
    0) good "the plan after import is already clean" ;;
    2)
      # The one outcome that must never pass. A plan that proposes destroying
      # and recreating a cluster nobody touched is the destructive wrong diff,
      # and the first version of this provider did exactly that: an imported
      # `cluster_type` held the platform's resolved reference while the
      # configuration held the head form, and the RequiresReplace on that
      # attribute turned the disagreement into a replacement. Asserting it here
      # is what keeps the fix honest.
      if grep -qE 'must be replaced|forces replacement|destroy and then create' "$RUN_DIR/import-plan.txt"; then
        # awk rather than `sed -n '/x/,+12p'`: the +N address form is GNU-only
        # and this script has to run on a Mac.
        awk '/must be replaced/ {show = 14} show-- > 0 { print "     " $0 }' "$RUN_DIR/import-plan.txt"
        die "the plan after import proposes REPLACING a running resource.
   Nothing changed but the state's provenance, so this is a provider bug, not a
   user error. Read internal/provider/clustertyperef.go."
      fi
      note "the plan proposes one change to the cluster, and it is expected:"
      awk '/will be updated in-place/ { show = 1 }
           show { print "     " $0 }
           show && /^    }/ { exit }' "$RUN_DIR/import-plan.txt"
      note "Two things converge, and neither is a defect:"
      note "  cluster_type moves from the platform's resolved reference to the head form"
      note "  the configuration uses. Note that it is an in-place update, NOT a"
      note "  replacement — getting that wrong destroys a running cluster."
      note "  node_pool narrows: import adopted every pool the platform materialized,"
      note "  and this configuration manages only the worker pool."
      note "Applying it touches no API — watch the request count."
      local before after
      before="$(wc -l < "$LOG_FILE")"
      tf apply -auto-approve -input=false -no-color \
        -var endpoint="$ENDPOINT" -var worker_nodes="$scaled" >/dev/null \
        || die "the reconciling apply failed"
      after="$(wc -l < "$LOG_FILE")"
      note "requests during that apply: $(( after - before )) (reads only — no PATCH, no scale)"
      set +e
      tf plan -detailed-exitcode -input=false -no-color \
        -var endpoint="$ENDPOINT" -var worker_nodes="$scaled" >/dev/null
      plan_status=$?
      set -e
      [ "$plan_status" = "0" ] || die "the plan after reconciling still proposes changes (exit $plan_status)"
      good "reconciled, and the plan is clean"
      ;;
    *) die "the plan after import failed with exit $plan_status" ;;
  esac

  # -------------------------------------------------------------------------
  step "8/8  terraform destroy"
  # -------------------------------------------------------------------------
  tf destroy -auto-approve -input=false -no-color \
    -var endpoint="$ENDPOINT" -var worker_nodes="$scaled" || die "terraform destroy failed"
  good "destroyed"

  # -------------------------------------------------------------------------
  step "What the fake saw"
  # -------------------------------------------------------------------------
  local total mints
  total="$(wc -l < "$LOG_FILE" | tr -d ' ')"
  mints="$(grep -c 'POST /v1/regions/global/auth/token' "$LOG_FILE" || true)"
  note "$total requests in total. The last 25:"
  awk '{ sub(/^[0-9\/]+ [0-9:]+ /, ""); print "   " $0 }' "$LOG_FILE" | tail -25
  note "full log: $LOG_FILE"
  note "$mints token mints for $total requests — one per Terraform command, because each"
  note "command is a fresh provider process. Within a process the token is minted once"
  note "and shared, which matters: a credential is allowed ten mints per minute."

  printf '\n%s%sPASS%s — against a fake. Read the header of this script for the caveats.\n' "$GREEN" "$B" "$R"
  printf '\nIterate: %smake demo-up%s, then edit %sexamples/demo/main.tf%s and run terraform there.\n\n' \
    "$B" "$R" "$B" "$R"
}

case "${1:-run}" in
  run)     cmd_run ;;
  setup|up) cmd_setup ;;
  cleanup|down) cmd_cleanup ;;
  env|demo-env) cmd_env ;;
  session) cmd_session ;;
  *)       die "unknown command ${1}; use run, setup, session, env or cleanup" ;;
esac
