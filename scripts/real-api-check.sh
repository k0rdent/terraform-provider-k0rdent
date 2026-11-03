#!/usr/bin/env bash
#
# The checks that need the real API, run on a local kind stack, and the record
# CI holds every pull request to.
#
#   scripts/real-api-check.sh run      fake drift, then make live, against the
#                                      stack. Writes ci/real-api/record.json
#                                      when both pass
#   scripts/real-api-check.sh verify   for every pull request: fails unless the
#                                      record names this tree's fake hash and
#                                      the API version in ci/real-api/api-version
#   scripts/real-api-check.sh verify-release [TREE]
#                                      for a release: the same, and the record
#                                      names TREE's code hash too. TREE is the
#                                      tagged commit's checkout, default this one
#   scripts/real-api-check.sh hash fake|code [TREE]
#                                      prints a hash
#
# The record holds two hashes, because it vouches for two things.
#
# The fake hash covers what decides whether the fake answers like the real API:
# the fake, the API client and its spec, fakedrift, the accepted differences,
# the lifecycle and this script. Every pull request is held to it, with the API
# version, so a change to the fake or a new API version needs a new run, and a
# change to the provider alone doesn't: its tests run against a fake that's
# known to match.
#
# The code hash covers everything the real API runs against: the fake hash's
# files, the provider (go.mod, go.sum, main.go, internal/ but its tests), make
# live and the walkthroughs. Only a release is held to it, so each release has run end to end
# against the real API without every merge asking for a run.
#
# Configuration for run, from the environment, with the names the provider
# reads:
#
#   K0RDENT_ENDPOINT        the stack's gateway, on localhost
#   K0RDENT_REGION          a region the stack serves
#   K0RDENT_PROJECT         a project to build in
#   K0RDENT_CLIENT_ID       a service account scoped to the project, with
#   K0RDENT_CLIENT_SECRET   compute-admin and credentials-admin, so an import
#                           can check SSH keys
#   REAL_API_ORG_CLIENT_ID      a service account scoped to the organization,
#   REAL_API_ORG_CLIENT_SECRET  with compute-admin, organizations-admin and
#                               iam-viewer, for the walkthroughs, as
#                               docs/walkthrough-hardware.md lists them
#   REAL_API_CLUSTER_TYPE   a cluster type in the region, with a worker pool
#                           and two or more available versions, so make live
#                           runs its upgrade steps
#   REAL_API_WALKTHROUGH_GAPS  what the stack can't do that a walkthrough
#                           asks for, from the list below. Default none
#   REAL_API_KUBE_CONTEXT   the stack's kube context, which the API version is
#                           read from. Default kind-k0rdent-rc5
#
# Status: 0 when it passed, 1 with what to do next on stderr otherwise.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
REPO="$PWD"
RECORD="$REPO/ci/real-api/record.json"
ACCEPTED="$REPO/ci/fake-drift-accepted.json"
LIFECYCLE="$REPO/ci/real-api/lifecycle"
# The fake's own seeded landscape.
FAKE_REGION=us-texas-1 FAKE_PROJECT=demo FAKE_CLUSTER_TYPE=gpu-training

# shellcheck source=lib/common.sh
. "$REPO/scripts/lib/common.sh"
# shellcheck source=lib/walkthrough.sh
. "$REPO/scripts/lib/walkthrough.sh"

FAKE_PATHS=(internal/tools/fakeapi internal/tools/fakedrift internal/k0rdent
  ci/fake-drift-accepted.json ci/real-api/lifecycle scripts/real-api-check.sh)
CODE_PATHS=("${FAKE_PATHS[@]}" go.mod go.sum main.go internal scripts/live.sh scripts/lib examples/live
  examples/walkthrough)

# tree_hash <fake|code> <tree> prints one hash over every file that hash
# covers, as <tree>'s working tree holds them, so a run before the commit and
# CI after it agree.
tree_hash() {
  local paths
  case "$1" in
    fake) paths=("${FAKE_PATHS[@]}") ;;
    code) paths=("${CODE_PATHS[@]}") ;;
    *) die "no hash named $1, want fake or code" ;;
  esac
  git -C "$2" ls-files --cached --others --exclude-standard -- "${paths[@]}" \
      ':!:*_test.go' ':!:internal/acceptance' ':!:internal/repocheck' \
    | LC_ALL=C sort -u \
    | while IFS= read -r f; do
        [ -f "$2/$f" ] && printf '%s %s\n' "$(git -C "$2" hash-object -- "$f")" "$f"
      done \
    | git hash-object --stdin
}

# want_api_version <tree> prints the API version <tree> is tested against.
want_api_version() {
  local file="$1/ci/real-api/api-version"
  [ -s "$file" ] || die "$file is missing. It names the API version the provider is tested against, such as v1.3.0-rc.5"
  tr -d '[:space:]' < "$file"
}

cmd_hash() {
  [ -n "${1:-}" ] || die "usage: scripts/real-api-check.sh hash fake|code [TREE]"
  tree_hash "$1" "${2:-$REPO}"
}

# check_record <tree> <fake|code>: fails unless <tree>'s record passed, names
# its API version, and names its fake hash, and its code hash too for code.
check_record() {
  local tree="$1" scope="$2" fake code="" want
  fake="$(tree_hash fake "$tree")" || die "could not hash $tree"
  [ "$scope" = code ] && { code="$(tree_hash code "$tree")" || die "could not hash $tree"; }
  want="$(want_api_version "$tree")" || exit 1
  [ -f "$tree/ci/real-api/record.json" ] \
    || die "no ci/real-api/record.json. Run make real-api-check against a kind stack running API $want, then commit the record."
  RECORD="$tree/ci/real-api/record.json" FAKE="$fake" CODE="$code" WANT="$want" python3 -c '
import json, os, sys
r = json.load(open(os.environ["RECORD"]))
fake, code, want = os.environ["FAKE"], os.environ["CODE"], os.environ["WANT"]
problems = []
if r.get("fake") != fake:
    problems.append("the fake, the API client or the real-API check changed since the last run (record %s, tree %s)" % (r.get("fake"), fake))
if code and r.get("code") != code:
    problems.append("this release has code the last run did not (record %s, tree %s)" % (r.get("code"), code))
if r.get("api_version") != want:
    problems.append("the last run was against API %s, and ci/real-api/api-version says %s" % (r.get("api_version"), want))
failed = [s for s, v in r.get("steps", {}).items() if v != "PASS"]
if failed or not r.get("steps"):
    problems.append("the record has steps that did not pass: %s" % (", ".join(failed) or "none recorded"))
if problems:
    for p in problems:
        print("real-api record: " + p, file=sys.stderr)
    print("Run make real-api-check against a kind stack running API %s, then commit ci/real-api/record.json." % want, file=sys.stderr)
    sys.exit(1)
gaps = r.get("landscape_gaps") or []
print("real-api record: API %s, run %s, covers this %s%s" % (want, r.get("ran"), "release" if code else "fake",
      ", without: " + ", ".join(gaps) if gaps else ""))'
}

# stack_api_version prints the one version every k0rdent API image on the
# stack is tagged with, and fails when they differ.
stack_api_version() {
  local ctx="${REAL_API_KUBE_CONTEXT:-kind-k0rdent-rc5}" tags
  tags="$(kubectl --context "$ctx" get deploy --all-namespaces \
      -o jsonpath='{range .items[*]}{range .spec.template.spec.containers[*]}{.image}{"\n"}{end}{end}' 2>/dev/null \
    | sed -n 's|.*/k0r-[a-z-]*:\(.*\)$|\1|p' | sort -u)" \
    || die "could not read the API's deployments from kube context $ctx. Set REAL_API_KUBE_CONTEXT to the stack's."
  [ -n "$tags" ] || die "kube context $ctx runs no k0rdent API image. Set REAL_API_KUBE_CONTEXT to the stack's."
  [ "$(printf '%s\n' "$tags" | wc -l | tr -d ' ')" = 1 ] \
    || die "the stack's API images carry more than one version: $(printf '%s' "$tags" | tr '\n' ' ')"
  printf '%s' "$tags"
}

# lifecycle <dir> <cluster type>: apply, re-plan, import both resources by
# their ids, plan, destroy. The provider reads its endpoint, region, project
# and credential from the environment. Each step's exit code goes to
# <dir>/steps.
lifecycle() {
  local dir="$1" ct="$2" region project rc
  region="$(eval printf '%s' "\"\${${ENV_PREFIX}REGION}\"")"
  project="$(eval printf '%s' "\"\${${ENV_PREFIX}PROJECT}\"")"
  mkdir -p "$dir"
  cp "$LIFECYCLE/main.tf" "$dir/"
  : > "$dir/steps"
  lc() { local name="$1"; shift; pace; ( cd "$dir" && "$TF_BIN" "$@" -no-color ) > "$dir/$name.log" 2>&1; rc=$?; printf '%s %s\n' "$name" "$rc" >> "$dir/steps"; }
  export TF_VAR_cluster_type="$ct" TF_VAR_ssh_public_key="$KEY"
  lc apply apply -auto-approve -input=false
  lc replan plan -detailed-exitcode -input=false
  # state rm reads no API, so it mints nothing and needs no pause.
  ( cd "$dir" && "$TF_BIN" state rm k0rdent_kubernetes_cluster.c k0rdent_instance_group.g ) > /dev/null 2>&1
  lc import-cluster import -input=false k0rdent_kubernetes_cluster.c "$region/$project/real-api-check-c"
  lc import-group import -input=false k0rdent_instance_group.g "$region/$project/real-api-check-g"
  lc plan-after-import plan -detailed-exitcode -input=false
  lc destroy destroy -auto-approve -input=false
  unset -f lc
}

# pace waits PACE seconds before a Terraform command against the stack: a
# credential gets ten token mints a minute, and each command mints once.
pace() { [ "${PACE:-0}" = 0 ] || sleep "$PACE"; }

# Landscape gaps: what a stack can't do that a walkthrough asks for, each
# named in REAL_API_WALKTHROUGH_GAPS and written into the record, so a pass
# says what it didn't cover. Each one changes a copy of a directory in one
# stated way. subnet-cidr drops a check, so only the real side's copy. The
# others turn a resource off, so both sides' copies, or the fake records
# requests the stack never got and the compare reads them as drift.
#
#   subnet-cidr    the stack answers a subnet's CIDR without allocating it from
#                  the pool, as kind does (10.0.0.0/24 for a /28 from any
#                  pool). full/ drops its check that each subnet is inside its
#                  pool
#   no-file-store  the stack runs no storage service, as kind doesn't.
#                  full-everything leaves file_store off
#   no-hosts       the stack has no server for an instance group's host, as
#                  kind has none. full-everything leaves instance_group off,
#                  and vpc_peering, which peers with the group's VPC
GAPS_KNOWN="subnet-cidr no-file-store no-hosts"

# gap <name> is true when REAL_API_WALKTHROUGH_GAPS names it.
gap() { case " ${REAL_API_WALKTHROUGH_GAPS:-} " in *" $1 "*) return 0 ;; esac; return 1; }

# apply_gaps <dir> <name> <side> changes the copy <dir> of walkthrough <name>
# for each gap named, and fails when the line it changes has moved. <side> is
# real or fake.
apply_gaps() {
  case "$2" in full | full-everything) ;; *) return 0 ;; esac
  if [ "$3" = real ] && gap subnet-cidr; then
    python3 - "$1/main.tf" <<'PY' || return 1
import sys
p = sys.argv[1]; s = open(p).read()
i = s.find('    postcondition {\n      condition = self.cidr != ""')
if i < 0:
    sys.exit("the subnet-cidr gap: full/main.tf has no subnet postcondition on self.cidr any more")
j = s.index("    }\n", s.index("error_message", i)) + 6
open(p, "w").write(s[:i] + s[j:])
PY
  fi
  if [ "$2" = full-everything ] && gap no-file-store; then
    switch_off "$1/terraform.tfvars" file_store no-file-store || return 1
  fi
  if [ "$2" = full-everything ] && gap no-hosts; then
    switch_off "$1/terraform.tfvars" instance_group no-hosts || return 1
    switch_off "$1/terraform.tfvars" vpc_peering no-hosts || return 1
  fi
}

# switch_off <terraform.tfvars> <switch> <gap> turns a build switch that's on
# off, and fails when it isn't on.
switch_off() {
  grep -Eq "^  $2 *= *true( |$)" "$1" \
    || { warn "the $3 gap: full-everything's terraform.tfvars has no $2 = true"; return 1; }
  sed -i.bak -E "s/^(  $2 *= *)true( |$)/\1false/" "$1"
}

# walkthroughs <dir> [real]: look/, then every directory of
# examples/walkthrough the fake can apply, as a reader runs them:
# terraform.tfvars filled in from what look/ printed, apply, plan again,
# destroy. The landscape gaps apply, as apply_gaps says. The credential is the one in
# the environment. Each step's exit code goes to <dir>/steps, as lifecycle's do.
walkthroughs() {
  local dir="$1" side="${2:-}" examples="$REPO/examples/walkthrough" d rc
  mkdir -p "$dir"
  : > "$dir/steps"
  wt() { local name="$1" sub="$2"; shift 2; pace; ( cd "$dir/$sub" && "$TF_BIN" "$@" -no-color ) > "$dir/$name.log" 2>&1; rc=$?; printf '%s %s\n' "$name" "$rc" >> "$dir/steps"; return "$rc"; }
  cp -R "$examples/look" "$dir/look"
  wt look look apply -auto-approve -input=false || return 0
  ( cd "$dir/look" && "$TF_BIN" output -json ) > "$dir/look.json" 2> "$dir/look-output.log" \
    || { printf 'look-output 1\n' >> "$dir/steps"; return 0; }
  walkthrough_pick "$dir/look.json"
  walkthrough_secrets "$dir"
  for d in $(walkthrough_dirs "$examples"); do
    [ "$d" = look ] && continue
    cp -R "$examples/${d%-everything}" "$dir/$d"
    walkthrough_tfvars "$d" "$examples" rapi > "$dir/$d/terraform.tfvars.new" \
      || { printf '%s-tfvars 1\n' "$d" >> "$dir/steps"; continue; }
    mv "$dir/$d/terraform.tfvars.new" "$dir/$d/terraform.tfvars"
    if ! apply_gaps "$dir/$d" "$d" "${side:-fake}" 2> "$dir/$d-gaps.log"; then
      printf '%s-gaps 1\n' "$d" >> "$dir/steps"
      continue
    fi
    if wt "$d-apply" "$d" apply -auto-approve -input=false; then
      wt "$d-replan" "$d" plan -detailed-exitcode -input=false
    fi
    wt "$d-destroy" "$d" destroy -auto-approve -input=false
  done
  wt look-destroy look destroy -auto-approve -input=false
  unset -f wt
  return 0
}

# lifecycle_passed <dir> <label> fails, naming the step and its log, unless
# every step exited 0.
lifecycle_passed() {
  local bad
  bad="$(awk '$2 != 0 {print $1 " (exit " $2 ")"}' "$1/steps")"
  if [ -z "$bad" ]; then
    good "$2: $(wc -l < "$1/steps" | tr -d ' ') steps passed"
    return 0
  fi
  warn "$2: $(printf '%s' "$bad" | tr '\n' ' ')"
  warn "logs in $1"
  return 1
}

cmd_run() {
  local endpoint want have fake code work fake_url proxy_pid fake_pid recorder steps_drift steps_live
  need kubectl; need go; need python3; need ssh-keygen
  read_names
  find_terraform
  endpoint="$(eval printf '%s' "\"\${${ENV_PREFIX}ENDPOINT:-}\"")"
  [ -n "$endpoint" ] || die "${ENV_PREFIX}ENDPOINT is not set. It is the kind stack's gateway, such as http://localhost:14000"
  case "$(endpoint_host "$endpoint")" in
    localhost:*|localhost|127.0.0.1|127.0.0.1:*) ;;
    *) die "${ENV_PREFIX}ENDPOINT is $endpoint. This check runs against a local kind stack only; make live is for other landscapes." ;;
  esac
  for v in REGION PROJECT CLIENT_ID CLIENT_SECRET; do
    [ -n "$(eval printf '%s' "\"\${${ENV_PREFIX}$v:-}\"")" ] || die "${ENV_PREFIX}$v is not set. TESTING.md lists what make real-api-check needs."
  done
  for v in REAL_API_ORG_CLIENT_ID REAL_API_ORG_CLIENT_SECRET; do
    [ -n "$(eval printf '%s' "\"\${$v:-}\"")" ] || die "$v is not set. TESTING.md lists what make real-api-check needs."
  done
  for g in ${REAL_API_WALKTHROUGH_GAPS:-}; do
    case " $GAPS_KNOWN " in *" $g "*) ;; *) die "REAL_API_WALKTHROUGH_GAPS names $g. The gaps are: $GAPS_KNOWN" ;; esac
  done
  [ -n "${REAL_API_CLUSTER_TYPE:-}" ] || die "REAL_API_CLUSTER_TYPE is not set. It is a cluster type id in ${ENV_PREFIX}REGION with a worker pool."

  want="$(want_api_version "$REPO")" || exit 1
  have="$(stack_api_version)" || exit 1
  [ "$have" = "$want" ] || die "the stack runs API $have, and ci/real-api/api-version says $want. Run against a stack at $want, or change the file if the provider now tracks $have."
  fake="$(tree_hash fake "$REPO")" || die "could not hash the tree"
  code="$(tree_hash code "$REPO")" || die "could not hash the tree"
  good "the stack runs API $have"

  work="$(private_tmpdir)" || exit 1
  trap '[ -n "${proxy_pid:-}" ] && kill $proxy_pid 2>/dev/null; [ -n "${fake_pid:-}" ] && kill "$fake_pid" 2>/dev/null' EXIT
  step "Build"
  build_provider "$work" 0.0.0-real-api
  export TF_CLI_CONFIG_FILE="$work/terraform.rc" TF_IN_AUTOMATION=1
  (cd "$REPO" && go build -o "$work/fakeapi" ./internal/tools/fakeapi && go build -o "$work/fakedrift" ./internal/tools/fakedrift) \
    || die "could not build the fake or fakedrift"
  ssh-keygen -q -t ed25519 -N '' -C real-api-check -f "$work/key" || die "could not make an SSH key"
  KEY="$(cat "$work/key.pub")"

  # recorder <upstream> <recording>: start fakedrift in front of upstream and
  # set recorder to its URL.
  recorder() {
    "$work/fakedrift" record -upstream "$1" -out "$2" > "$2.url" 2> "$2.log" &
    proxy_pid="${proxy_pid:-} $!"
    for _ in $(seq 1 50); do [ -s "$2.url" ] && break; sleep 0.1; done
    recorder="$(head -1 "$2.url")"
    [ -n "$recorder" ] || die "fakedrift did not start: $(cat "$2.log")"
  }

  step "Fake drift: the lifecycle against the fake"
  start_loopback_server "$work/fakeapi" -secrets-file "$work/fake-secrets.env" "$work/fake.url" "$work/fake.log" \
    || die "the fake did not start: $(cat "$work/fake.log")"
  fake_pid="$SERVER_PID"
  fake_url="$SERVER_URL"
  recorder "$fake_url" "$work/fake.ndjson"
  (
    export_fake_credential "$work" "$work/fake-secrets.env"
    export "${ENV_PREFIX}ENDPOINT=$recorder" "${ENV_PREFIX}REGION=$FAKE_REGION" "${ENV_PREFIX}PROJECT=$FAKE_PROJECT"
    export TF_CLI_CONFIG_FILE="$work/terraform.rc"
    lifecycle "$work/fake" "$FAKE_CLUSTER_TYPE"
    unset "${ENV_PREFIX}REGION" "${ENV_PREFIX}PROJECT"
    walkthroughs "$work/fake-walkthrough"
  )
  steps_drift=PASS
  lifecycle_passed "$work/fake" "the lifecycle against the fake" || steps_drift=FAIL
  lifecycle_passed "$work/fake-walkthrough" "the walkthroughs against the fake" || steps_drift=FAIL

  step "Fake drift: the lifecycle against the real API"
  recorder "$endpoint" "$work/real.ndjson"
  (
    export "${ENV_PREFIX}ENDPOINT=$recorder" PACE="${REAL_API_MINT_PACE:-7}"
    lifecycle "$work/real" "$REAL_API_CLUSTER_TYPE"
    # The walkthroughs as a reader with an organization's account runs them:
    # the endpoint and the credential only.
    export "${ENV_PREFIX}CLIENT_ID=$REAL_API_ORG_CLIENT_ID" "${ENV_PREFIX}CLIENT_SECRET=$REAL_API_ORG_CLIENT_SECRET"
    unset "${ENV_PREFIX}REGION" "${ENV_PREFIX}PROJECT"
    walkthroughs "$work/real-walkthrough" real
  )
  lifecycle_passed "$work/real" "the lifecycle against the real API" || steps_drift=FAIL
  lifecycle_passed "$work/real-walkthrough" "the walkthroughs against the real API" || steps_drift=FAIL

  step "Fake drift: compare"
  if "$work/fakedrift" compare -spec "$REPO/internal/k0rdent/spec/openapi.yaml" \
      -fake "$work/fake.ndjson" -real "$work/real.ndjson" -accepted "$ACCEPTED" > "$work/compare.txt"; then
    good "$(tail -1 "$work/compare.txt")"
  else
    steps_drift=FAIL
    cat "$work/compare.txt"
    warn "fix the fake to answer as the real API does, or add a line with the reason to ci/fake-drift-accepted.json"
  fi

  step "make live against the stack"
  # make live mints a token per command, and the fake drift legs just used
  # some of the minute's.
  sleep "${REAL_API_MINT_PAUSE:-60}"
  steps_live=PASS
  TF_VAR_cluster_type_id="${TF_VAR_cluster_type_id:-$REAL_API_CLUSTER_TYPE}" \
    LIVE_CONFIRM="$(endpoint_host "$endpoint")" "$REPO/scripts/live.sh" run || steps_live=FAIL
  # A skipped step proves nothing, so the record needs every step run.
  if grep -q '^SKIP' "$REPO/.live/results.txt" 2>/dev/null; then
    steps_live=FAIL
    warn "make live skipped a step: $(grep '^SKIP' "$REPO/.live/results.txt" | tr -s ' ' | tr '\n' ' ')"
  fi

  if [ "$steps_drift" != PASS ] || [ "$steps_live" != PASS ]; then
    die "fake drift $steps_drift, live $steps_live. ci/real-api/record.json is unchanged, so CI still asks for a run."
  fi
  RECORD="$RECORD" FAKE="$fake" CODE="$code" API="$have" GAPS="${REAL_API_WALKTHROUGH_GAPS:-}" TF="$("$TF_BIN" version | head -1)" python3 -c '
import json, os, datetime
json.dump({
    "fake": os.environ["FAKE"],
    "code": os.environ["CODE"],
    "api_version": os.environ["API"],
    "terraform": os.environ["TF"],
    "steps": {"fake drift": "PASS", "live": "PASS"},
    "landscape_gaps": os.environ["GAPS"].split(),
    "ran": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
}, open(os.environ["RECORD"], "w"), indent=2)
open(os.environ["RECORD"], "a").write("\n")'
  good "wrote ci/real-api/record.json. Commit it with the change."
}

case "${1:-}" in
  run) cmd_run ;;
  verify) check_record "$REPO" fake ;;
  verify-release) check_record "${2:-$REPO}" code ;;
  hash) cmd_hash "${2:-}" "${3:-}" ;;
  *) die "usage: scripts/real-api-check.sh run|verify|verify-release [TREE]|hash fake|code [TREE]" ;;
esac
