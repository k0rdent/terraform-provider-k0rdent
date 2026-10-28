#!/usr/bin/env bash
#
# Tests what scripts/live-storage.sh does around Terraform, with Terraform
# and go as shims, so nothing here needs a network, a credential or a cluster:
#
#   * No KUBE_* variable reaches Terraform, because the kubernetes provider
#     falls back to them when the handed-over host is null.
#   * A failed run whose full destroy fails, as it does when the kubeconfig
#     can't open for a `failed` cluster, drops the kubernetes resources from
#     state and destroys the cluster with a targeted destroy.
#   * When that fails too, the result says the cluster may still exist.
#
# Each case runs a copy of the script in a throwaway tree, so nothing it
# writes touches this checkout.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "live-storage script test: FAIL - $1" >&2; exit 1; }

mkdir -p "$work/bin"
cat > "$work/bin/go" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  run)   printf 'Provider type name             : k0rdent\nRegistry namespace             : k0rdent\n' ;;
  env)   echo test ;;
  build) while [ $# -gt 0 ]; do [ "$1" = -o ] && { : > "$2"; break; }; shift; done ;;
  *)     echo test ;;
esac
EOF
# The Terraform shim writes each call's argv, and every KUBE_* it can see, to
# the calls directory. The apply always fails, which is what sends the run to
# its cleanup. A full destroy fails the way an unopenable kubeconfig fails it.
# TARGETED_DESTROY says how a targeted one ends.
cat > "$work/bin/tf" <<EOF
#!/usr/bin/env bash
n="\$(ls "$work/calls" | grep -c '^argv\.' || true)"
printf '%s\n' "\$@" > "$work/calls/argv.\$n"
env | grep '^KUBE' >> "$work/calls/kube-env" || true
case "\$1" in
  init) exit 0 ;;
  apply) echo "Error: Could not download the kubeconfig"; exit 1 ;;
  state)
    case "\$2" in
      list) printf '%s\n' data.k0rdent_cluster_type.chosen k0rdent_kubernetes_cluster.storage \
                        kubernetes_persistent_volume_claim_v1.probe kubernetes_pod_v1.probe ;;
      rm) exit 0 ;;
    esac ;;
  destroy)
    case "\$*" in
      *-target=*) [ "\$TARGETED_DESTROY" = ok ] ;;
      *) echo "Error: Could not download the kubeconfig"; exit 1 ;;
    esac ;;
esac
EOF
chmod +x "$work/bin/go" "$work/bin/tf"

# run_case <TARGETED_DESTROY> runs the script's `run` against a dummy https
# endpoint, with KUBE_* set the way a shell pointed at another cluster would
# have them.
run_case() {
  rm -rf "$work/repo" "$work/calls"
  mkdir -p "$work/repo/scripts/tests" "$work/repo/examples/live/storage" "$work/calls"
  cp "$here/scripts/live-storage.sh" "$work/repo/scripts/"
  cp -R "$here/scripts/lib" "$work/repo/scripts/"
  set +e
  env -i PATH="$work/bin:$PATH" HOME="$work" TF_BIN="$work/bin/tf" LIVE_MINT_PACE=0 \
    K0RDENT_ENDPOINT=https://example.invalid K0RDENT_REGION=r1 K0RDENT_PROJECT=p1 \
    K0RDENT_API_KEY=dummy-not-a-secret LIVE_CONFIRM=example.invalid LIVE_CLUSTER=tf-live-storage-test \
    KUBE_HOST=https://127.0.0.1:6443 KUBE_TOKEN=dummy-not-a-secret KUBE_CONFIG_PATH=/dev/null KUBE_CTX=kind \
    KUBECONFIG=/dev/null TARGETED_DESTROY="$1" \
    "$work/repo/scripts/live-storage.sh" run > "$work/out" 2>&1 < /dev/null
  status=$?
  set -e
  results="$work/repo/.live/storage/results.txt"
}

# argv_of <n> prints call n's argv on one line.
argv_of() { tr '\n' ' ' < "$work/calls/argv.$1"; }

# call_numbers prints the number of every recorded call, in order.
call_numbers() {
  for argv_file in "$work/calls"/argv.*; do
    [ -e "$argv_file" ] && echo "${argv_file##*.}"
  done | sort -n
}

# Case 1: the fallback destroys the cluster.

run_case ok
[ "$status" != 0 ] || { cat "$work/out" >&2; fail "a run whose apply failed exited 0"; }
[ ! -s "$work/calls/kube-env" ] || { cat "$work/calls/kube-env" >&2; fail "a KUBE_* variable reached Terraform"; }

calls="$(for call in $(call_numbers); do argv_of "$call"; echo; done)"
printf '%s\n' "$calls" | grep -q '^destroy .*' || { echo "$calls" >&2; fail "no full destroy was tried"; }
printf '%s\n' "$calls" | grep -qx 'state rm -no-color kubernetes_persistent_volume_claim_v1.probe kubernetes_pod_v1.probe ' \
  || { echo "$calls" >&2; fail "the kubernetes resources weren't dropped from state, and only they"; }
printf '%s\n' "$calls" | grep -q -- '-target=k0rdent_kubernetes_cluster.storage' \
  || { echo "$calls" >&2; fail "no destroy was targeted at the cluster"; }

# In that order: the full destroy, then state rm, then the targeted destroy.
order="$(printf '%s\n' "$calls" | awk '/^destroy/ && !/-target/ {print "full"} /^state rm/ {print "rm"} /-target=/ {print "targeted"}' | tr '\n' ' ')"
[ "$order" = "full rm targeted " ] || { echo "$calls" >&2; fail "the cleanup ran as \"$order\", not full, rm, targeted"; }
grep -q '^PASS  destroy .*targeted at the cluster after the full destroy failed' "$results" \
  || { cat "$results" >&2; fail "the result doesn't say the targeted destroy removed the cluster"; }

# Case 2: when the fallback fails too, the result says so.

run_case fail
grep -q '^FAIL  destroy .*may still exist' "$results" \
  || { cat "$results" >&2; fail "a failed fallback isn't recorded as a cluster that may still exist"; }

echo 'live-storage script test: PASS'
