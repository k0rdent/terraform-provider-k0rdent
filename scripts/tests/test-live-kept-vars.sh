#!/usr/bin/env bash
#
# Tests what scripts/live.sh keeps when LIVE_KEEP=1, with Terraform and go as
# shims, so nothing here needs a network, a credential or a cluster:
#
#   * .live/kept.tfvars names the cluster type as well as its version, so the
#     drift recipe in TESTING.md can't land on whichever type sorts first in
#     the catalog by then.
#   * When kept.tfvars can't be written, the EXIT trap warns and carries on:
#     the destroy is still recorded as skipped and the results still print.
#
# Each case runs a copy of the script in a throwaway tree, so nothing it
# writes touches this checkout.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "live kept-vars test: FAIL - $1" >&2; exit 1; }

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
# The apply fails, which sends the run to its EXIT trap with LIVE_KEEP=1.
# BREAK_KEPT=1 makes kept.tfvars a directory first, so it can't be written.
cat > "$work/bin/tf" <<EOF
#!/usr/bin/env bash
case "\$1" in
  version) echo "Terraform v1.99.0" ;;
  apply)
    [ "\${BREAK_KEPT:-0}" = 1 ] && mkdir -p "$work/repo/.live/kept.tfvars"
    echo "Error: the apply failed on purpose"; exit 1 ;;
esac
EOF
chmod +x "$work/bin/go" "$work/bin/tf"

# run_case <BREAK_KEPT> runs `run` with LIVE_KEEP=1 against a dummy https
# endpoint, with the cluster type and version set the way the upgrade step
# sets them.
run_case() {
  rm -rf "$work/repo"
  mkdir -p "$work/repo/scripts" "$work/repo/examples/live"
  cp "$here/scripts/live.sh" "$work/repo/scripts/"
  cp -R "$here/scripts/lib" "$work/repo/scripts/"
  set +e
  env -i PATH="$work/bin:$PATH" HOME="$work" TF_BIN="$work/bin/tf" LIVE_MINT_PACE=0 \
    K0RDENT_ENDPOINT=https://example.invalid K0RDENT_REGION=r1 K0RDENT_PROJECT=p1 \
    K0RDENT_API_KEY=dummy-not-a-secret LIVE_CONFIRM=example.invalid LIVE_CLUSTER=tf-live-kept-test \
    LIVE_KEEP=1 LIVE_UPGRADE=0 TF_VAR_cluster_type_id=ct-picked TF_VAR_cluster_type_version=ver-oldest \
    BREAK_KEPT="$1" \
    "$work/repo/scripts/live.sh" run > "$work/out" 2>&1 < /dev/null
  status=$?
  set -e
  kept="$work/repo/.live/kept.tfvars"
  results="$work/repo/.live/results.txt"
}

# --- 1. kept.tfvars names the type and the version -------------------------

run_case 0
[ "$status" != 0 ] || { cat "$work/out" >&2; fail "a run whose apply failed exited 0"; }
[ -f "$kept" ] || { cat "$work/out" >&2; fail "LIVE_KEEP=1 wrote no kept.tfvars"; }
grep -qx 'cluster_type_id = "ct-picked"' "$kept" || { cat "$kept" >&2; fail "kept.tfvars doesn't name the cluster type"; }
grep -qx 'cluster_type_version = "ver-oldest"' "$kept" || { cat "$kept" >&2; fail "kept.tfvars doesn't name the version"; }
grep -qx 'cluster_name = "tf-live-kept-test"' "$kept" || { cat "$kept" >&2; fail "kept.tfvars doesn't name the cluster"; }
grep -q '^SKIP  destroy .*LIVE_KEEP=1' "$results" || { cat "$results" >&2; fail "the kept destroy isn't recorded as skipped"; }

# --- 2. an unwritable kept.tfvars warns, and the trap still finishes ---------

run_case 1
grep -q 'could not be written' "$work/out" || { cat "$work/out" >&2; fail "an unwritable kept.tfvars gave no warning"; }
grep -q '^SKIP  destroy .*LIVE_KEEP=1' "$results" || { cat "$results" >&2; fail "the trap stopped before recording the skipped destroy"; }
grep -q 'Results: ' "$work/out" || { cat "$work/out" >&2; fail "the trap stopped before printing the results"; }

echo 'live kept-vars test: PASS'
