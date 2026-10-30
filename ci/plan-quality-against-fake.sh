#!/usr/bin/env bash
#
# Two promises the walkthrough makes, held for every directory in
# examples/walkthrough against the fake:
#
#   1. After an apply, `terraform plan -refresh-only` finds nothing: no
#      resource drifted, no output moved. The plan's JSON is what's read,
#      because Terraform 1.16 exits 2 from -detailed-exitcode on a
#      refresh-only plan of any configuration with a data source, even when
#      it reports no changes. A directory with no data source has to exit 0.
#   2. Plans show as few "known after apply" values as they can. The first
#      plan of each directory, and in full/ a plan that changes one
#      display_name and one that edits one security group rule, are counted
#      against ci/unknown-budget.json by internal/tools/unknownbudget, which
#      names the address and attribute that went over.
#
# UNKNOWN_BUDGET_WRITE=1 rewrites the budget from the plans instead, keeping
# the reasons already written. TESTING.md says when and how.
#
# nodes/ needs a real cluster's API server, so it isn't planned here, as in
# ci/walkthrough-against-fake.sh.
#
# Requires: go, python3, and `terraform` or `tofu` on PATH.

set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"

# shellcheck source=../scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"
# shellcheck source=../scripts/lib/walkthrough.sh
. "$REPO/scripts/lib/walkthrough.sh"

read_names
find_terraform
need python3

WORK="$(private_tmpdir)" || exit 1
FAKE_PID=""
cleanup() {
  [ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf '\nplan quality: FAIL - %s\n' "$1" >&2; exit 1; }

EXAMPLES="$REPO/examples/walkthrough"
BUDGET="$REPO/ci/unknown-budget.json"
PLANS="$WORK/plans"
mkdir -p "$PLANS"

build_provider "$WORK" 0.0.0-plan-quality
go build -o "$WORK/fakeapi" ./internal/tools/fakeapi
go build -o "$WORK/unknownbudget" ./internal/tools/unknownbudget

start_loopback_server "$WORK/fakeapi" -secrets-file "$WORK/fake-secrets.env" "$WORK/fake.url" "$WORK/fake.log" \
  || { cat "$WORK/fake.log"; fail "the fake did not start"; }
FAKE_PID="$SERVER_PID"
export_fake_credential "$WORK" "$WORK/fake-secrets.env"
export "${ENV_PREFIX}ENDPOINT=$SERVER_URL"
unset "${ENV_PREFIX}REGION" "${ENV_PREFIX}API_KEY"

# tf <command> [args...] runs in $DIR and keeps the output in $DIR/last.log.
tf() {
  ( cd "$DIR" && "$TF_BIN" "$1" -no-color "${@:2}" ) > "$DIR/last.log" 2>&1 \
    || { cat "$DIR/last.log" >&2; return 1; }
}

step "look"
DIR="$WORK/look"
cp -R "$EXAMPLES/look" "$DIR"
tf apply -auto-approve -input=false || fail "the look apply failed"

tf output -json
walkthrough_pick "$DIR/last.log"
good "picked $cluster_type in $region, organization $organization, project $project"

walkthrough_secrets "$WORK"

# plan_saved <scenario>: plan to a file and keep its JSON for the budget.
plan_saved() {
  local scenario="$1"
  tf plan -input=false -out=plan.bin || fail "$scenario: the plan failed"
  ( cd "$DIR" && "$TF_BIN" show -json plan.bin ) > "$PLANS/$scenario.json" || fail "$scenario: terraform show -json failed"
}

# refresh_clean <scenario> <when>: a refresh-only plan proposes nothing.
refresh_clean() {
  local scenario="$1" when="$2"

  set +e
  ( cd "$DIR" && "$TF_BIN" plan -refresh-only -detailed-exitcode -input=false -no-color -out=refresh.bin ) > "$DIR/refresh.log" 2>&1
  status=$?
  set -e
  [ "$status" = 0 ] || [ "$status" = 2 ] \
    || { cat "$DIR/refresh.log" >&2; fail "$scenario: the refresh-only plan $when failed"; }

  # Any drifted attribute, moved output or planned change fails.
  ( cd "$DIR" && "$TF_BIN" show -json refresh.bin ) | python3 -c '
import json, sys
p, status = json.load(sys.stdin), sys.argv[1]
found = []
for d in p.get("resource_drift", []):
    b, a = d["change"].get("before") or {}, d["change"].get("after") or {}
    moved = [k for k in sorted(set(b) | set(a)) if b.get(k) != a.get(k)] or ["(the object)"]
    for k in moved:
        found.append("drift: %s.%s: %r to %r" % (d["address"], k, b.get(k), a.get(k)))
for name, c in sorted(p.get("output_changes", {}).items()):
    if c.get("actions") != ["no-op"]:
        found.append("output: %s %s" % (name, c.get("actions")))
for rc in p.get("resource_changes", []):
    if rc["change"]["actions"] != ["no-op"]:
        found.append("change: %s %s" % (rc["address"], rc["change"]["actions"]))
data = any(r.get("mode") == "data" for r in p.get("prior_state", {}).get("values", {}).get("root_module", {}).get("resources", []))
if status == "2" and not data and not found:
    found.append("exit 2 with no data source and nothing reported")
print("\n".join(found))
sys.exit(1 if found else 0)
' "$status" >&2 || { cat "$DIR/refresh.log" >&2; fail "$scenario: the refresh-only plan $when found changes"; }
  good "refresh-only plan $when: no drift, no output moved"
}

# run_dir <scenario> [<address whose display_name changes>
#         [<the change plan's summary line, when it isn't one update in place>]]
# The scenario is a walkthrough directory, or full-everything.
run_dir() {
  local scenario="$1" example="$1" display_name_address="${2:-}" change_summary="${3:-}"
  [ "$scenario" = full-everything ] && example=full

  DIR="$WORK/$scenario"
  step "$scenario"
  cp -R "$EXAMPLES/$example" "$DIR"
  walkthrough_tfvars "$scenario" "$EXAMPLES" ci > "$DIR/terraform.tfvars.new" || fail "$scenario: no terraform.tfvars"
  mv "$DIR/terraform.tfvars.new" "$DIR/terraform.tfvars"
  plan_saved "$scenario"
  tf apply -auto-approve -input=false plan.bin || fail "$scenario: the apply failed"
  refresh_clean "$scenario" "after the apply"

  if [ -n "$display_name_address" ]; then
    python3 - "$DIR/main.tf" "$display_name_address" <<'PY' || fail "$scenario: no display_name on $display_name_address to change"
import re, sys
path, addr = sys.argv[1], sys.argv[2]
typ, name = addr.split(".")
src = open(path).read()
start = src.index('resource "%s" "%s" {' % (typ, name))
m = re.compile(r'^(  display_name += )(.+)$', re.M).search(src, start)
open(path, "w").write(src[:m.start()] + m.group(1) + '"${' + m.group(2) + '}, changed"' + src[m.end():])
PY
    plan_saved "$scenario-change"
    want="${change_summary:-Plan: 0 to add, 1 to change, 0 to destroy.}"
    grep -qF "$want" "$DIR/last.log" \
      || { cat "$DIR/last.log" >&2; fail "$scenario: the change plan doesn't say \"$want\""; }
    [ -z "$change_summary" ] || warn "$scenario: the change plans \"$change_summary\", not one update in place"
    tf apply -auto-approve -input=false plan.bin || fail "$scenario: the change failed"
    refresh_clean "$scenario" "after the change"
  fi

  # full/ only: one security group rule edited, an egress rule's port.
  if [ "$example" = full ]; then
    python3 - "$DIR/main.tf" <<'PY' || fail "$scenario: no egress rule to edit"
import sys
path = sys.argv[1]
src = open(path).read()
old = '{ name = "https", protocol = "tcp", port = "443" }'
if src.count(old) != 1:
    sys.exit(1)
open(path, "w").write(src.replace(old, old.replace('"443"', '"8443"')))
PY
    plan_saved "$scenario-rule-change"
    grep -qF "Plan: 0 to add, 1 to change, 0 to destroy." "$DIR/last.log" \
      || { cat "$DIR/last.log" >&2; fail "$scenario: the rule change plan isn't one update in place"; }
    tf apply -auto-approve -input=false plan.bin || fail "$scenario: the rule change failed"
    refresh_clean "$scenario" "after the rule change"
  fi

  tf destroy -auto-approve -input=false || fail "$scenario: the destroy failed"
  good "planned, applied, refreshed and destroyed"
}

run_dir cluster
run_dir cluster-own-project
run_dir network
run_dir full k0rdent_kubernetes_cluster.this

# Renaming the cluster here replaces no VPC peering. The cluster's pending
# change defers every data source that depends on it to the apply, so
# remote_vpc takes the hosts' VPC id from the instance group, which stays
# known, rather than a self_link from data.k0rdent_vpc.owned.
run_dir full-everything k0rdent_kubernetes_cluster.this

step "Unknowns against ci/unknown-budget.json"

# PLAN_QUALITY_KEEP=<directory> keeps the plans' JSON, to read what went over.
if [ -n "${PLAN_QUALITY_KEEP:-}" ]; then
  mkdir -p "$PLAN_QUALITY_KEEP" && cp "$PLANS"/*.json "$PLAN_QUALITY_KEEP/"
fi

if [ "${UNKNOWN_BUDGET_WRITE:-0}" = 1 ]; then
  "$WORK/unknownbudget" -budget "$BUDGET" -plans "$PLANS" -write
  printf '\n%severy walkthrough directory refreshes clean. Review the budget with git diff.%s\n' "$GREEN" "$R"
  exit 0
else
  "$WORK/unknownbudget" -budget "$BUDGET" -plans "$PLANS" || fail "a plan is outside the budget"
fi

printf '\n%severy walkthrough directory refreshes clean and plans within the budget.%s\n' "$GREEN" "$R"
