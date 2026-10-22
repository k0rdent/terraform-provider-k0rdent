#!/usr/bin/env bash
#
# Against a REAL control plane: a volume claim binds through the cluster's
# own storage class, reached through the kubeconfig the provider hands over.
#
#   scripts/live-storage.sh validate   init and validate examples/live/storage.
#                                      Needs no endpoint and creates nothing
#   scripts/live-storage.sh run        the run. CREATES REAL RESOURCES
#
# `run` creates a cluster on whatever deployment K0RDENT_ENDPOINT names, a
# claim and a pod inside it, and destroys all of it again. It refuses to start
# unless LIVE_CONFIRM names that endpoint's host, and it always attempts a
# destroy on the way out, including after a failure.
#
# CONFIGURATION, from the environment, the same names the provider reads:
#
#   K0RDENT_ENDPOINT       required. Base URL, scheme and host only, no /v1
#   K0RDENT_REGION         required. A deployable region id
#   K0RDENT_PROJECT        required. The project's id
#   K0RDENT_CLIENT_ID and K0RDENT_CLIENT_SECRET, or K0RDENT_API_KEY
#
#   LIVE_CONFIRM           `run` only. Must equal the endpoint's host
#   LIVE_CLUSTER           cluster id. Default: tf-live-storage-<epoch>
#   LIVE_KEEP=1            skip the destroy, to inspect what was made
#   LIVE_WORKERS           worker nodes. Default 1, and it can't be 0
#
# TF_VAR_storage_class_name, TF_VAR_namespace, TF_VAR_claim_size,
# TF_VAR_probe_image and TF_VAR_cluster_type_id reach the configuration as
# they are. See its variables.
#
# WHAT THIS PROVES
#   That a workload's claim binds through the cluster's own CSI path, and that
#   the provider's part of it is only the connection. The apply fails if the
#   claim doesn't read Bound or the pod can't mount it, and the run fails if
#   the configuration's check block says the claim names no volume. Results go
#   to .live/storage/results.txt, one PASS, FAIL or SKIP line per step, in the
#   shape `make live` writes.
#
# On a kind stack (MOCK_MODE=true) the kubeconfig names a mock server and its
# CA is a placeholder, so the kubernetes provider has nothing to connect to.
# The run fails there, and says that's the stack and not the provider.

set -uo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"
STORAGE_DIR="$REPO/examples/live/storage"
RUN_DIR="$REPO/.live/storage"
RESULTS="$RUN_DIR/results.txt"

# shellcheck source=lib/common.sh
. "$REPO/scripts/lib/common.sh"

# The kubernetes provider doesn't fail when its `host` is null. It falls back
# to KUBE_HOST, KUBE_TOKEN, KUBE_CONFIG_PATH(S), KUBE_CTX and the rest, so a
# shell pointed at a kind or management cluster would have the claim bind
# there and the run pass for the wrong cluster. None of them reaches Terraform
# from here. The configuration's postcondition on the kubeconfig is the other
# half of this.
for kube_var in $(env | sed -n 's/^\(KUBE_[A-Za-z0-9_]*\)=.*/\1/p'); do
  unset "$kube_var"
done
unset KUBECONFIG kube_var

# record <STATUS> <step> <detail>, the same columns scripts/live.sh writes.
record() {
  private_dir "$RUN_DIR"
  printf '%-5s %-22s %s\n' "$1" "$2" "${3:-}" >> "$RESULTS"
}

# A credential is allowed ten token mints a minute, and every Terraform command
# here mints once. The kubeconfig open mints a second token for the cluster.
MINT_PACE="${LIVE_MINT_PACE:-6}"
pace() { sleep "$MINT_PACE"; }

# Terraform's data directory goes in the run directory, so neither a provider
# download nor a state lock lands next to the configuration.
build_storage_provider() {
  private_dir "$RUN_DIR"
  build_provider "$RUN_DIR" 0.0.0-live
  export TF_CLI_CONFIG_FILE="$RUN_DIR/terraform.rc"
  export TF_DATA_DIR="$RUN_DIR/tfdata"
  export TF_IN_AUTOMATION=1
}

tf() { ( cd "$STORAGE_DIR" && "$TF_BIN" "$@" ); }

# destroy_all <log> destroys what the run made. It returns 0 when a full
# destroy did it, 1 when only the fallback below did, and 2 when the cluster
# may still exist.
#
# A full destroy opens the kubeconfig to configure the kubernetes provider,
# even with no kubernetes resource left in state, and the open fails for a
# cluster that's `failed` or never issued one. Then nothing is destroyed and
# the cluster is left holding machines. So on a failure the kubernetes
# resources are dropped from state, and a destroy targeted at the cluster
# alone runs. That one doesn't open the kubeconfig. The claim and the pod go
# with the cluster, though a volume whose class retains it can outlive it on
# the storage backend.
destroy_all() {
  local log="$1"
  if tf destroy -auto-approve -input=false -no-color "${VARS[@]}" >"$log" 2>&1; then
    return 0
  fi
  warn "the full destroy failed. Dropping the kubernetes resources from state and destroying the cluster alone"
  local addrs
  addrs="$(tf state list 2>>"$log" | grep -E '^kubernetes_' || true)"
  if [ -n "$addrs" ]; then
    # shellcheck disable=SC2086 # one address per word
    tf state rm -no-color $addrs >>"$log" 2>&1 || { warn "terraform state rm failed, see $log"; return 2; }
  fi
  if tf destroy -auto-approve -input=false -no-color "${VARS[@]}" \
       -target="${PROVIDER_NAME}_kubernetes_cluster.storage" >>"$log" 2>&1; then
    return 1
  fi
  return 2
}

# The kubernetes provider comes from the registry. The k0rdent one is the
# dev override, which init skips and says so.
tf_init() {
  tf init -backend=false -input=false -no-color >"$RUN_DIR/init.txt" 2>&1
}

read_config() {
  ENDPOINT="$(eval printf '%s' "\"\${${ENV_PREFIX}ENDPOINT:-}\"")"
  REGION="$(eval printf '%s' "\"\${${ENV_PREFIX}REGION:-}\"")"
  PROJECT="$(eval printf '%s' "\"\${${ENV_PREFIX}PROJECT:-}\"")"

  [ -n "$ENDPOINT" ] || die "${ENV_PREFIX}ENDPOINT is not set. It is the API base URL, scheme and host only."
  endpoint_is_encrypted "$ENDPOINT" || die "${ENV_PREFIX}ENDPOINT is $ENDPOINT, and the credential goes in the body of the token request.

   Use https. http is accepted only for localhost or a loopback IP, which is
   where a kind stack and the local fake run. The provider refuses the same."
  [ -n "$REGION" ]  || die "${ENV_PREFIX}REGION is not set. Run 'make live-preflight' to list the region ids this deployment serves."
  [ -n "$PROJECT" ] || die "${ENV_PREFIX}PROJECT is not set. It is the project's id."
  HOST="$(endpoint_host "$ENDPOINT")"
}

cmd_validate() {
  read_names; find_terraform
  step "Validate examples/live/storage"
  rm -rf "$RUN_DIR/tfdata"
  build_storage_provider
  tf_init || { cat "$RUN_DIR/init.txt" >&2; die "terraform init failed"; }
  good "initialised, the kubernetes provider from the registry and $PROVIDER_NAME from the build"
  tf validate -no-color || die "terraform validate failed"
  rm -f "$STORAGE_DIR/.terraform.lock.hcl"
  good "examples/live/storage validates"
}

cmd_run() {
  read_names; find_terraform; read_config

  [ -n "${LIVE_CONFIRM:-}" ] || die "make live-storage creates REAL resources on $ENDPOINT.

   Confirm the deployment by naming its host:

       LIVE_CONFIRM=$HOST make live-storage"
  [ "$LIVE_CONFIRM" = "$HOST" ] || die "LIVE_CONFIRM is \"$LIVE_CONFIRM\" but ${ENV_PREFIX}ENDPOINT's host is \"$HOST\". They have to match."

  CLUSTER="${LIVE_CLUSTER:-tf-live-storage-$(date +%s)}"
  WORKERS="${LIVE_WORKERS:-1}"
  [ "$WORKERS" -ge 1 ] 2>/dev/null || die "LIVE_WORKERS is \"$WORKERS\". The pod needs a node, so it has to be 1 or more."
  VARS=(-var "cluster_name=$CLUSTER" -var "worker_nodes=$WORKERS")

  private_dir "$RUN_DIR"
  : > "$RESULTS"
  {
    printf 'k0rdent AI Terraform provider, live storage run\n'
    printf 'date:       %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf 'endpoint:   %s\n' "$ENDPOINT"
    printf 'region:     %s\n' "$REGION"
    printf 'project:    %s\n' "$PROJECT"
    printf 'cluster:    %s, %s worker(s)\n' "$CLUSTER" "$WORKERS"
    printf 'class:      %s\n' "${TF_VAR_storage_class_name:-the cluster default}"
    printf '\n'
  } >> "$RESULTS"

  # Not `local`: the EXIT trap runs after this function returns.
  destroyed=0
  cleanup() {
    local rc=$?
    trap - EXIT INT TERM
    if [ "$destroyed" = "1" ]; then
      :
    elif [ "${LIVE_KEEP:-0}" = "1" ]; then
      warn "LIVE_KEEP=1, leaving $CLUSTER in place. Destroy it with:"
      warn "  TF_CLI_CONFIG_FILE=$RUN_DIR/terraform.rc TF_DATA_DIR=$RUN_DIR/tfdata $TF_BIN -chdir=$STORAGE_DIR destroy -var cluster_name=$CLUSTER -var worker_nodes=$WORKERS"
      record SKIP destroy "LIVE_KEEP=1"
    else
      printf '\n%s━━ Cleaning up %s\n' "$B" "$R"
      note "attempting a destroy even though the run did not finish"
      pace
      local how=0
      destroy_all "$RUN_DIR/cleanup.txt" || how=$?
      case "$how" in
        0) good "destroyed $CLUSTER"
           record PASS destroy "on cleanup"
           destroyed=1 ;;
        1) good "destroyed $CLUSTER by a targeted destroy, after the full one failed"
           record PASS destroy "on cleanup, targeted at the cluster after the full destroy failed, see $RUN_DIR/cleanup.txt"
           destroyed=1 ;;
        *) warn "the cleanup destroy FAILED. $CLUSTER may still exist on $HOST. See $RUN_DIR/cleanup.txt"
           record FAIL destroy "cleanup destroy failed, $CLUSTER may still exist" ;;
      esac
    fi
    if [ "$destroyed" = "1" ] && [ "${LIVE_KEEP:-0}" != "1" ]; then
      rm -f "$STORAGE_DIR/terraform.tfstate" "$STORAGE_DIR"/terraform.tfstate.*.backup \
            "$STORAGE_DIR/terraform.tfstate.backup" "$STORAGE_DIR/.terraform.lock.hcl"
      rm -rf "$RUN_DIR/tfdata"
    fi
    printf '\n%sResults: %s%s\n' "$DIM" "$RESULTS" "$R"
    exit $rc
  }
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  printf '%sk0rdent AI Terraform provider, LIVE storage run%s\n' "$B" "$R"
  warn "this creates real resources on $HOST and destroys them again"
  note "cluster id: $CLUSTER"

  step "1/5  Build the provider and fetch the kubernetes provider"
  rm -f "$STORAGE_DIR/terraform.tfstate" "$STORAGE_DIR/terraform.tfstate.backup"
  rm -rf "$RUN_DIR/tfdata"
  build_storage_provider
  if tf_init; then
    record PASS build "and terraform init"
  else
    cat "$RUN_DIR/init.txt" >&2
    record FAIL build "terraform init failed, see $RUN_DIR/init.txt"
    die "terraform init failed"
  fi

  step "2/5  terraform apply: the cluster, the kubeconfig, the claim and the pod"
  note "the claim's create waits for Bound and the pod's for Running"
  pace
  local apply_out="$RUN_DIR/apply.txt"
  set +e
  tf apply -auto-approve -input=false -no-color "${VARS[@]}" 2>&1 | tee "$apply_out"
  local apply_status=${PIPESTATUS[0]}
  set -e
  if [ "$apply_status" != "0" ]; then
    explain_failure "$apply_out"
    record FAIL apply "terraform apply exit $apply_status, see $apply_out"
    die "terraform apply failed"
  fi
  record PASS apply "cluster $CLUSTER, then a claim and a pod through the handed-over kubeconfig"

  step "3/5  The claim, as the cluster bound it"
  local claim claim_status
  set +e
  claim="$(tf output -no-color -json claim 2>/dev/null | python3 -c '
import json, sys
c = json.load(sys.stdin)
print("%s/%s bound to %s through class %s" % (c["namespace"], c["name"], c.get("volume") or "nothing", c.get("storage_class") or "none"))
sys.exit(0 if c.get("volume") and c.get("storage_class") else 1)
')"
  claim_status=$?
  set -e
  note "$claim"
  if [ "$claim_status" != "0" ]; then
    record FAIL bound "$claim"
    die "the claim is not bound to a volume"
  fi
  if grep -q 'Check block assertion failed' "$apply_out"; then
    grep -A3 'Check block assertion failed' "$apply_out" | sed 's/^/     /'
    record FAIL check "a check block failed on apply, see $apply_out"
    die "a check block failed on apply. Full output: $apply_out"
  fi
  good "$claim"
  record PASS bound "$claim"

  step "4/5  terraform plan: nothing to change, and both checks hold on a fresh read"
  note "this opens the kubeconfig during the plan, with a token minted for it"
  pace
  local plan_out="$RUN_DIR/replan.txt"
  set +e
  tf plan -detailed-exitcode -input=false -no-color "${VARS[@]}" >"$plan_out" 2>&1
  local plan_status=$?
  set -e
  if grep -q 'Check block assertion failed' "$plan_out"; then
    record FAIL replan "a check block failed on a fresh read, see $plan_out"
    die "a check block failed on the re-plan. Full output: $plan_out"
  fi
  case "$plan_status" in
    0) good "exit 0, no changes, the claim still names its volume and the pod still runs"
       record PASS replan "exit 0, no changes, claim_bound and pod_running hold" ;;
    2) record FAIL replan "exit 2, plan proposes changes after a no-op apply, see $plan_out"
       die "the plan proposes changes after an apply that changed nothing. Full output: $plan_out" ;;
    *) explain_failure "$plan_out"
       record FAIL replan "terraform plan exit $plan_status, see $plan_out"
       die "terraform plan failed with exit $plan_status. Full output: $plan_out" ;;
  esac

  step "5/5  terraform destroy: the pod and the claim, then the cluster"
  if [ "${LIVE_KEEP:-0}" = "1" ]; then
    warn "LIVE_KEEP=1, not destroying $CLUSTER"
    record SKIP destroy "LIVE_KEEP=1"
    destroyed=1
  else
    pace
    local how=0
    destroy_all "$RUN_DIR/destroy.txt" || how=$?
    case "$how" in
      0) good "destroyed"
         record PASS destroy ;;
      # The cluster is gone, but a full destroy of a healthy run failing is a
      # finding in its own right.
      1) record FAIL destroy "the full destroy failed, then a destroy targeted at the cluster removed it, see $RUN_DIR/destroy.txt"
         destroyed=1
         die "the full destroy failed. A targeted destroy removed $CLUSTER. Full output: $RUN_DIR/destroy.txt" ;;
      *) record FAIL destroy "terraform destroy failed, see $RUN_DIR/destroy.txt"
         die "terraform destroy failed. $CLUSTER may still exist on $HOST. Full output: $RUN_DIR/destroy.txt" ;;
    esac
    destroyed=1
  fi

  step "Results"
  cat "$RESULTS"
  if grep -q '^FAIL' "$RESULTS"; then
    printf '\n%s%sFAIL%s against %s. See the lines above.\n\n' "$RED" "$B" "$R" "$HOST"
    exit 1
  fi
  printf '\n%s%sPASS%s against %s: a claim bound through the cluster'"'"'s own storage class.\n\n' \
    "$GREEN" "$B" "$R" "$HOST"
}

# explain_failure <log> names the deployment gaps a failed apply or plan most
# often comes down to, so a run on the wrong kind of stack says so.
explain_failure() {
  if grep -qE 'mock\.k0rdent\.local|no such host' "$1"; then
    warn "the kubeconfig's server doesn't resolve. On a kind stack in MOCK_MODE it names a"
    warn "mock server and there's no cluster behind it. This run needs a real deployment."
  fi
  if grep -qiE 'certificate|x509|PEM' "$1"; then
    warn "the kubernetes provider couldn't use the CA. A MOCK_MODE kubeconfig carries a"
    warn "placeholder, not a certificate. This run needs a real deployment."
  fi
  if grep -qE 'timeout while waiting for state|context deadline exceeded' "$1"; then
    warn "something didn't settle in time. A claim stuck Pending usually means no default"
    warn "StorageClass (set TF_VAR_storage_class_name) or no CSI provisioner behind it."
  fi
}

case "${1:-run}" in
  run)      cmd_run ;;
  validate) cmd_validate ;;
  *)        die "unknown command ${1}. Use run or validate" ;;
esac
