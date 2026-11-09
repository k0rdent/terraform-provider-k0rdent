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
#   scripts/real-api-check.sh hash fake|code|fake-list|code-list|hashing [TREE]
#                                      prints a hash, a digest of what one covers,
#                                      or the hashing version
#   scripts/real-api-check.sh hardware look/ and full/ with the switches for
#                                      what kind can't do, against the fake
#                                      and a landscape with hardware. CREATES
#                                      REAL RESOURCES, for hours. Writes
#                                      ci/real-api/hardware-record.json when
#                                      every step passes. TESTING.md lists
#                                      what it reads
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
# live and the walkthroughs. Only a release is held to it, so each release has
# run end to end against the real API without every merge asking for a run.
#
# ci/real-api/paths lists what each hash covers, and this script reads it
# from the tree it hashes.
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

# HASHING is how tree_hash and list_hash make a hash. A record names the one
# it was made with. Changing either function breaks every committed record on
# every branch, since a release branch's tag is checked by main's script: bump
# HASHING with it, so the check says why instead of blaming the code.
HASHING=1

# read_path_lists <tree> sets PATHS_FAKE, PATHS_CODE and PATHS_SKIP from
# <tree>'s ci/real-api/paths, which says what each hash covers. They're data
# in the tree, so main's script hashes a release branch's tag by that branch's
# own lists. An entry has to be one path that matches a file in <tree>: a typo
# or a comment after it would otherwise shrink what a hash covers, silently.
read_path_lists() {
  local file="$1/ci/real-api/paths" kind path
  [ -s "$file" ] || die "$1 has no ci/real-api/paths, which says what the real-API record's hashes cover. A tree from before it can't be checked: add the file and run make real-api-check on it."
  PATHS_FAKE=() PATHS_CODE=() PATHS_SKIP=()
  while read -r kind path; do
    case "$kind" in ''|'#'*) continue ;; esac
    case "$path" in
      '') die "$file: the $kind line names no path" ;;
      *'#'* | *[[:space:]]*) die "$file: \"$kind $path\" has a # or a space in its path. Put one path a line, and comments on lines of their own" ;;
    esac
    [ -n "$(git -C "$1" ls-files --cached --others --exclude-standard -- "$path" 2>/dev/null | head -1)" ] \
      || die "$file: \"$kind $path\" matches no file in $1. Fix the path, or drop the line"
    case "$kind" in
      fake) PATHS_FAKE+=("$path") ;;
      code) PATHS_CODE+=("$path") ;;
      skip) PATHS_SKIP+=("$path") ;;
      *) die "$file: a line starts with $kind, want fake, code or skip" ;;
    esac
  done < "$file"
  [ "${#PATHS_FAKE[@]}" -gt 0 ] || die "$file lists nothing for the fake hash"
}

# list_paths <fake|code> prints, one a line, the entries that hash covers and
# skips, as read_path_lists set them.
list_paths() {
  local p
  for p in "${PATHS_FAKE[@]}"; do printf 'fake %s\n' "$p"; done
  if [ "$1" = code ]; then
    for p in ${PATHS_CODE[@]+"${PATHS_CODE[@]}"}; do printf 'code %s\n' "$p"; done
  fi
  for p in ${PATHS_SKIP[@]+"${PATHS_SKIP[@]}"}; do printf 'skip %s\n' "$p"; done
}

# tree_hash <fake|code> <tree> prints one hash over every file that hash
# covers, as <tree>'s working tree holds them, so a run before the commit and
# CI after it agree.
tree_hash() {
  local paths=() kind p
  case "$1" in fake|code) ;; *) die "no hash named $1, want fake or code" ;; esac
  read_path_lists "$2"
  while read -r kind p; do
    case "$kind" in skip) paths+=(":!:$p") ;; *) paths+=("$p") ;; esac
  done < <(list_paths "$1")
  git -C "$2" ls-files --cached --others --exclude-standard -- "${paths[@]}" \
    | LC_ALL=C sort -u \
    | while IFS= read -r f; do
        [ -f "$2/$f" ] && printf '%s %s\n' "$(git -C "$2" hash-object -- "$f")" "$f"
      done \
    | git hash-object --stdin
}

# list_hash <fake|code> <tree> prints a hash of the paths that hash covers, as
# <tree>'s ci/real-api/paths lists them, in any order.
list_hash() {
  case "$1" in fake|code) ;; *) die "no list named $1, want fake or code" ;; esac
  read_path_lists "$2"
  list_paths "$1" | LC_ALL=C sort -u | git hash-object --stdin
}

# want_api_version <tree> prints the API version <tree> is tested against.
want_api_version() {
  local file="$1/ci/real-api/api-version"
  [ -s "$file" ] || die "$file is missing. It names the API version the provider is tested against, such as v1.3.0-rc.5"
  tr -d '[:space:]' < "$file"
}

cmd_hash() {
  case "${1:-}" in
    fake|code) tree_hash "$1" "${2:-$REPO}" ;;
    fake-list|code-list) list_hash "${1%-list}" "${2:-$REPO}" ;;
    hashing) printf '%s\n' "$HASHING" ;;
    *) die "usage: scripts/real-api-check.sh hash fake|code|fake-list|code-list|hashing [TREE]" ;;
  esac
}

# check_record <tree> <fake|code>: fails unless <tree>'s record passed, names
# its API version, and names its fake hash, and its code hash too for code.
check_record() {
  local tree="$1" scope="$2" fake code="" want fake_list code_list=""
  fake="$(tree_hash fake "$tree")" || die "could not hash $tree"
  fake_list="$(list_hash fake "$tree")" || die "could not hash $tree's ci/real-api/paths"
  if [ "$scope" = code ]; then
    code="$(tree_hash code "$tree")" || die "could not hash $tree"
    code_list="$(list_hash code "$tree")" || die "could not hash $tree's ci/real-api/paths"
  fi
  want="$(want_api_version "$tree")" || exit 1
  [ -f "$tree/ci/real-api/record.json" ] \
    || die "no ci/real-api/record.json. Run make real-api-check against a kind stack running API $want, then commit the record."
  RECORD="$tree/ci/real-api/record.json" HW="$tree/ci/real-api/hardware-record.json" \
    GAP_RESOURCES="$GAP_RESOURCES" FAKE="$fake" CODE="$code" WANT="$want" \
    FAKE_LIST="$fake_list" CODE_LIST="$code_list" HASHING="$HASHING" python3 -c '
import json, os, sys
r = json.load(open(os.environ["RECORD"]))
e = os.environ
fake, code, want = e["FAKE"], e["CODE"], e["WANT"]
lists = r.get("lists") or {}
problems = []
# What a hash covers, and how it is made, are checked before the hash, so a
# change to either is named as that and not as a change to the code.
if str(r.get("hashing")) != e["HASHING"]:
    problems.append("the record was hashed by version %s of the hashing, and this script hashes by version %s, so the hashes cannot match" % (r.get("hashing", "none"), e["HASHING"]))
elif lists.get("fake") != e["FAKE_LIST"]:
    problems.append("the paths the fake hash covers, in ci/real-api/paths, changed since the last run (record %s, tree %s)" % (lists.get("fake"), e["FAKE_LIST"]))
elif r.get("fake") != fake:
    problems.append("the fake, the API client or the real-API check changed since the last run (record %s, tree %s)" % (r.get("fake"), fake))
if code and str(r.get("hashing")) == e["HASHING"]:
    if lists.get("code") != e["CODE_LIST"]:
        problems.append("the paths the code hash covers, in ci/real-api/paths, changed since the last run (record %s, tree %s)" % (lists.get("code"), e["CODE_LIST"]))
    elif r.get("code") != code:
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
      ", without: " + ", ".join(gaps) if gaps else ""))
if not code:
    sys.exit(0)
# A release says plainly which resources only kind ran, and whether a
# hardware run covers them. It does not fail on it: the release decides.
by_gap = dict(g.split(":", 1) for g in os.environ["GAP_RESOURCES"].split())
kind_only = sorted({res for g in gaps for res in by_gap.get(g, g).split(",")})
covered = []
try:
    hw = json.load(open(os.environ["HW"]))
except FileNotFoundError:
    hw = None
if hw is None:
    print("hardware record: none")
elif hw.get("code") != code:
    print("hardware record: API %s on %s, run %s, ran other code than this release" % (hw.get("api_version"), hw.get("landscape"), hw.get("ran")))
else:
    covered = hw.get("covered") or []
    print("hardware record: API %s (%s) on %s, run %s, covers this release: %s" % (hw.get("api_version"),
          hw.get("api_version_source"), hw.get("landscape"), hw.get("ran"), ", ".join(covered)))
    if hw.get("api_version") != want:
        print("hardware record: ran API %s, and ci/real-api/api-version says %s" % (hw.get("api_version"), want))
missing = [res for res in kind_only if res not in covered]
if missing:
    print("kind only, no hardware for: " + ", ".join(missing))'
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

# tf_step <run dir> <name> <directory> <terraform args...> runs Terraform in
# <directory>, its output in <run dir>/<name>.log, and adds "<name> <exit>" to
# <run dir>/steps. With HEARTBEAT set, as in hardware mode, it also prints a
# line when the command starts and ends, one every HEARTBEAT seconds while it
# runs, and the step's result to RESULTS. Terraform then runs in the
# background as TF_PID, so a signal reaches the script's trap at once.
tf_step() {
  local run="$1" name="$2" sub="$3" log="$1/$2.log" start=$SECONDS next rc
  shift 3
  if [ -z "${HEARTBEAT:-}" ]; then
    ( cd "$sub" && "$TF_BIN" "$@" -no-color ) > "$log" 2>&1
    rc=$?
    printf '%s %s\n' "$name" "$rc" >> "$run/steps"
    return "$rc"
  fi
  TF_STEP="${run##*/}/$name"
  say "$TF_STEP started. Log: $log"
  # Terraform runs in its own process group, so a terminal's ctrl-C reaches
  # only this script, and hw_cleanup sends Terraform one interrupt. Two make it
  # exit at once, which can lose the state of what it's creating.
  ( cd "$sub" && exec python3 -c 'import os, sys; os.setpgrp(); os.execvp(sys.argv[1], sys.argv[1:])' \
      "$TF_BIN" "$@" -no-color ) > "$log" 2>&1 &
  TF_PID=$!
  next=$((SECONDS + HEARTBEAT))
  while kill -0 "$TF_PID" 2>/dev/null; do
    sleep 1
    if [ "$SECONDS" -ge "$next" ]; then
      say "  $TF_STEP: $(took $((SECONDS - start))) so far, $(still_doing "$log")"
      next=$((SECONDS + HEARTBEAT))
    fi
  done
  wait "$TF_PID"
  rc=$?
  TF_PID=""
  printf '%s %s\n' "$name" "$rc" >> "$run/steps"
  if [ "$rc" = 0 ]; then
    say "$TF_STEP passed in $(took $((SECONDS - start)))"
    result PASS "$TF_STEP" "$(took $((SECONDS - start)))"
  else
    say "$TF_STEP FAILED (exit $rc) after $(took $((SECONDS - start))). Log: $log"
    grep -E '^Error|│ Error' "$log" | head -3 | sed 's/^/     /'
    result FAIL "$TF_STEP" "exit $rc after $(took $((SECONDS - start))), $log"
  fi
  return "$rc"
}

# say prints a line with the time, for a run that takes hours.
say() { printf '%s[%s]%s %s\n' "$DIM" "$(date +%H:%M:%S)" "$R" "$1"; }

# took <seconds> prints a duration such as 1h52m, 12m or 40s.
took() {
  if [ "$1" -ge 3600 ]; then printf '%dh%02dm' $(($1 / 3600)) $(($1 % 3600 / 60))
  elif [ "$1" -ge 60 ]; then printf '%dm' $(($1 / 60))
  else printf '%ds' "$1"; fi
}

# still_doing <log> names what Terraform's "Still creating..." lines say it's
# waiting on, or says it hasn't named anything yet.
still_doing() {
  awk '
    /: Still (creating|destroying|modifying|reading)\.\.\./ { a = $1; sub(/:$/, "", a); v = $3; sub(/\.\.\.$/, "", v); st[a] = v }
    /: (Creation|Destruction|Modifications|Read) complete/ { a = $1; sub(/:$/, "", a); delete st[a] }
    END { n = 0; for (a in st) printf "%s%s %s", (n++ ? ", " : "still "), st[a], a; if (!n) printf "no resource waiting yet" }
  ' "$1"
}

# result <status> <step> [detail] adds a line to RESULTS, when it's set.
result() {
  [ -n "${RESULTS:-}" ] || return 0
  printf '%-5s %-40s %s\n' "$1" "$2" "${3:-}" >> "$RESULTS"
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
# The resources each gap leaves without a run on the real API, which a release
# names unless a hardware run covers them.
GAP_RESOURCES="subnet-cidr:k0rdent_subnet no-file-store:k0rdent_file_store no-hosts:k0rdent_instance_group,k0rdent_vpc_peering"

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

# walkthroughs <dir> [real|hw-fake|hw-real]: look/, then every directory of
# examples/walkthrough the fake can apply, as a reader runs them:
# terraform.tfvars filled in from what look/ printed, apply, plan again,
# destroy. On kind the landscape gaps apply, as apply_gaps says. With hw-fake
# and hw-real, only full-everything runs, with hardware mode's build switches,
# and hw-real builds where the runner said, not where look/ pointed. The
# credential is the one in the environment. Each step's exit code goes to
# <dir>/steps, as lifecycle's do.
walkthroughs() {
  local dir="$1" side="${2:-}" examples="$REPO/examples/walkthrough" d rc keep=0
  [ "$side" = hw-real ] && [ "${REAL_API_KEEP:-0}" = 1 ] && keep=1
  mkdir -p "$dir"
  : > "$dir/steps"
  wt() { local name="$1" sub="$2"; shift 2; pace; tf_step "$dir" "$name" "$dir/$sub" "$@"; }
  cp -R "$examples/look" "$dir/look"
  wt look look apply -auto-approve -input=false || return 0
  ( cd "$dir/look" && "$TF_BIN" output -json ) > "$dir/look.json" 2> "$dir/look-output.log" \
    || { printf 'look-output 1\n' >> "$dir/steps"; return 0; }
  walkthrough_pick "$dir/look.json"
  [ "$side" = hw-real ] && hw_pick
  walkthrough_secrets "$dir"
  for d in $(walkthrough_dirs "$examples"); do
    [ "$d" = look ] && continue
    case "$side" in hw-*) [ "$d" = full-everything ] || continue ;; esac
    cp -R "$examples/${d%-everything}" "$dir/$d"
    walkthrough_tfvars "$d" "$examples" "${HW_ID:-rapi}" > "$dir/$d/terraform.tfvars.new" \
      || { printf '%s-tfvars 1\n' "$d" >> "$dir/steps"; continue; }
    mv "$dir/$d/terraform.tfvars.new" "$dir/$d/terraform.tfvars"
    # Kind's gaps; hardware mode sets its own switches below.
    if [ "${side#hw-}" = "$side" ] && ! apply_gaps "$dir/$d" "$d" "${side:-fake}" 2> "$dir/$d-gaps.log"; then
      printf '%s-gaps 1\n' "$d" >> "$dir/steps"
      continue
    fi
    case "$side" in
      hw-*)
        if ! hw_tfvars "$dir/$d/terraform.tfvars" "$side" 2> "$dir/$d-switches.log"; then
          printf '%s-switches 1\n' "$d" >> "$dir/steps"
          result FAIL "${dir##*/}/$d-switches" "$(cat "$dir/$d-switches.log")"
          continue
        fi ;;
    esac
    if wt "$d-apply" "$d" apply -auto-approve -input=false; then
      wt "$d-replan" "$d" plan -detailed-exitcode -input=false
    fi
    [ "$keep" = 1 ] || wt "$d-destroy" "$d" destroy -auto-approve -input=false
  done
  [ "$keep" = 1 ] || wt look-destroy look destroy -auto-approve -input=false
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

# stack_leftovers <endpoint> prints, one a line, the objects in the project
# and the organization that look/ or the walkthroughs would read: clusters,
# groups, VPCs, networks, security groups, file stores, webhook subscriptions
# and other projects. A passing run removes its own, so on a stack only this
# check uses, each came from an interrupted run, and would turn up as a
# difference the fake has no part in. It reads with the organization's
# service account, and fails on any answer but a 200 with JSON, so a list it
# can't read never passes as an empty one.
stack_leftovers() {
  local endpoint="$1" tok org p region project out
  region="$(eval printf '%s' "\"\${${ENV_PREFIX}REGION}\"")"
  project="$(eval printf '%s' "\"\${${ENV_PREFIX}PROJECT}\"")"
  tok="$(token_request_body "$REAL_API_ORG_CLIENT_ID" "$REAL_API_ORG_CLIENT_SECRET" "" \
    | curl -sS -m 15 -X POST "$endpoint/v1/regions/global/auth/token" -H 'Content-Type: application/json' --data-binary @- 2>/dev/null \
    | jq -r '.access_token // empty' 2>/dev/null)"
  [ -n "$tok" ] || die "REAL_API_ORG_CLIENT_ID and REAL_API_ORG_CLIENT_SECRET don't mint a token on $endpoint. The env file make real-api-stack writes sets them."
  # read_list <path> prints the answer's body, and dies unless it's a 200
  # with a JSON object.
  read_list() {
    local resp code body
    resp="$(curl_bearer "$tok" -sS -m 15 -w '\n%{http_code}' "$endpoint$1" 2>/dev/null)"
    code="${resp##*$'\n'}" body="${resp%$'\n'*}"
    [ "$code" = 200 ] || die "reading $1 to check the stack is clean answered ${code:-nothing}, not 200, so what an earlier run left can't be counted. Check REAL_API_ORG_CLIENT_ID's roles and the stack, then run this again."
    printf '%s' "$body" | jq -e 'type == "object"' >/dev/null 2>&1 \
      || die "reading $1 to check the stack is clean answered 200 with no JSON object, so what an earlier run left can't be counted."
    printf '%s' "$body"
  }
  # ids <path> <label> prints "<label> <id>" for each item of the answer's
  # lists.
  ids() {
    local body
    body="$(read_list "$1")" || exit 1
    printf '%s' "$body" | jq -r --arg l "$2" '.[] | arrays | .[] | .id // empty | "\($l) \(.)"'
  }
  for p in compute/clusters compute/instance-groups networking/vpcs networking/networks \
      networking/security-groups storage/file-stores; do
    ids "/v1/regions/$region/projects/$project/$p" "${p#*/}"
  done
  ids /v1/regions/global/notifications/webhooks webhook
  out="$(read_list /v1/regions/global/organizations)" || exit 1
  org="$(printf '%s' "$out" | jq -r '.organizations[0].id // empty')"
  [ -n "$org" ] || die "the organization's service account sees no organization on $endpoint."
  out="$(ids "/v1/regions/global/organizations/$org/projects" project)" || exit 1
  [ -z "$out" ] || printf '%s\n' "$out" | grep -vx "project $project" || true
  unset -f ids read_list
  return 0
}

# recorder <upstream> <recording>: start the caller's $work/fakedrift in front
# of upstream, add it to proxy_pid and set recorder to its URL.
recorder() {
  "$work/fakedrift" record -upstream "$1" -out "$2" > "$2.url" 2> "$2.log" &
  proxy_pid="${proxy_pid:-} $!"
  for _ in $(seq 1 50); do [ -s "$2.url" ] && break; sleep 0.1; done
  recorder="$(head -1 "$2.url")"
  [ -n "$recorder" ] || die "fakedrift did not start: $(cat "$2.log")"
}

cmd_run() {
  local endpoint want have fake code work fake_url proxy_pid fake_pid recorder steps_drift steps_live leftovers
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
  leftovers="$(stack_leftovers "$endpoint")" || exit 1
  [ -z "$leftovers" ] || die "the stack holds what an earlier run left: $(printf '%s' "$leftovers" | paste -sd, - | sed 's/,/, /g'). look/ would read it, and the comparison would count it. Rebuild the stack clean with REAL_API_FRESH=1 make real-api-stack, source the env file it writes, and run this again."
  good "the stack holds nothing from an earlier run"

  work="$(private_tmpdir)" || exit 1
  trap '[ -n "${proxy_pid:-}" ] && kill $proxy_pid 2>/dev/null; [ -n "${fake_pid:-}" ] && kill "$fake_pid" 2>/dev/null' EXIT
  step "Build"
  build_provider "$work" 0.0.0-real-api
  export TF_CLI_CONFIG_FILE="$work/terraform.rc" TF_IN_AUTOMATION=1
  (cd "$REPO" && go build -o "$work/fakeapi" ./internal/tools/fakeapi && go build -o "$work/fakedrift" ./internal/tools/fakedrift) \
    || die "could not build the fake or fakedrift"
  ssh-keygen -q -t ed25519 -N '' -C real-api-check -f "$work/key" || die "could not make an SSH key"
  KEY="$(cat "$work/key.pub")"

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
    good "$(grep -m1 ' operations compared, ' "$work/compare.txt")"
    # An operation only one side made isn't compared, so it's listed.
    sed -n '/^recorded on the /,$p' "$work/compare.txt"
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
  RECORD="$RECORD" FAKE="$fake" CODE="$code" API="$have" GAPS="${REAL_API_WALKTHROUGH_GAPS:-}" TF="$("$TF_BIN" version | head -1)" \
    FAKE_LIST="$(list_hash fake "$REPO")" CODE_LIST="$(list_hash code "$REPO")" HASHING="$HASHING" python3 -c '
import json, os, datetime
json.dump({
    "fake": os.environ["FAKE"],
    "code": os.environ["CODE"],
    "hashing": int(os.environ["HASHING"]),
    "lists": {"fake": os.environ["FAKE_LIST"], "code": os.environ["CODE_LIST"]},
    "api_version": os.environ["API"],
    "terraform": os.environ["TF"],
    "steps": {"fake drift": "PASS", "live": "PASS"},
    "landscape_gaps": os.environ["GAPS"].split(),
    "ran": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
}, open(os.environ["RECORD"], "w"), indent=2)
open(os.environ["RECORD"], "a").write("\n")'
  good "wrote ci/real-api/record.json. Commit it with the change."
}

# --- Hardware mode ------------------------------------------------------------
#
# A kind stack allocates no subnet from a pool, runs no storage service and has
# no host for an instance group, so those resources never meet the real API in
# `run`. `hardware` runs look/ and full-everything on a landscape with
# hardware, against the fake too, through the same recorder. TESTING.md lists
# what it reads.

HW_RECORD="$REPO/ci/real-api/hardware-record.json"
# full/'s build switches hardware mode turns on. webhook and own_project stay
# off: kind covers both, a webhook to an address nobody serves gets turned off
# partway through a long run, and a project of its own needs
# organizations-admin.
HW_BUILD_ON="network security_group vpc_security_groups instance_group file_store vpc_peering"
HW_KIND_COVERS="webhook own_project"

# hw_api_version sets HW_API and HW_API_SOURCE from REAL_API_VERSION, or from
# the API images in REAL_API_KUBE_CONTEXT, and fails when both are set and
# differ.
hw_api_version() {
  local read=""
  if [ -n "${REAL_API_KUBE_CONTEXT:-}" ]; then
    read="$(stack_api_version)" || return 1
  fi
  if [ -n "${REAL_API_VERSION:-}" ] && [ -n "$read" ] && [ "$REAL_API_VERSION" != "$read" ]; then
    die "REAL_API_VERSION says $REAL_API_VERSION, and the API images in kube context $REAL_API_KUBE_CONTEXT carry $read"
  fi
  if [ -n "$read" ]; then
    HW_API="$read" HW_API_SOURCE="read from the API's images"
  elif [ -n "${REAL_API_VERSION:-}" ]; then
    HW_API="$REAL_API_VERSION" HW_API_SOURCE="stated by the runner"
  else
    die "no API version. Set REAL_API_VERSION to the landscape's API version, such as v1.3.0-rc.5, or REAL_API_KUBE_CONTEXT to a kube context that reads the API's deployments. The record says which."
  fi
}

# hw_pick points the walkthrough at what the runner named, not at what look/
# listed first: on a shared landscape the first project or cluster type is
# someone else's.
hw_pick() {
  # shellcheck disable=SC2034 # read by walkthrough_tfvars
  region="$HW_REGION" project="$HW_PROJECT" cluster_type="$REAL_API_CLUSTER_TYPE" filtered_type="$REAL_API_CLUSTER_TYPE"
  # shellcheck disable=SC2034 # read by walkthrough_tfvars
  [ -z "${REAL_API_ORGANIZATION:-}" ] || organization="$REAL_API_ORGANIZATION"
}

# hw_tfvars <terraform.tfvars> <side> turns full/'s build switches in HW_ON on
# and every other one off, and on hw-real sets REAL_API_ADDRESS_POOL. It fails
# when a switch it turns on isn't in the file.
hw_tfvars() {
  local pool=""
  [ "$2" = hw-real ] && pool="${REAL_API_ADDRESS_POOL:-}"
  ON="$HW_ON" POOL="$pool" python3 - "$1" <<'PY'
import os, re, sys
path = sys.argv[1]
on = set(os.environ["ON"].split())
out, seen, in_build = [], set(), False
for line in open(path).read().splitlines():
    if line.startswith("build = {"):
        in_build = True
    elif in_build and line.startswith("}"):
        in_build = False
    elif in_build:
        m = re.match(r"^(\s+)([a-z_]+)(\s*=\s*)(true|false)\b(.*)$", line)
        if m:
            seen.add(m.group(2))
            line = m.group(1) + m.group(2) + m.group(3) + ("true" if m.group(2) in on else "false") + m.group(5)
    out.append(line)
if on - seen:
    sys.exit("full/terraform.tfvars has no build switch %s any more" % ", ".join(sorted(on - seen)))
if os.environ["POOL"]:
    out.append('address_pool = "%s"' % os.environ["POOL"])
open(path, "w").write("\n".join(out) + "\n")
PY
}

# hw_mint prints a token for the landscape's credential.
hw_mint() {
  local response
  response="$(token_request_body "$HW_CLIENT_ID" "$HW_CLIENT_SECRET" "" | curl -sS -m 30 -X POST \
    "$HW_ENDPOINT/v1/regions/global/auth/token" -H 'Content-Type: application/json' --data-binary @-)" \
    || { echo "could not reach $HW_ENDPOINT" >&2; return 1; }
  RESP="$response" python3 -c '
import json, os, sys
try:
    tok = json.loads(os.environ["RESP"]).get("access_token")
except Exception:
    tok = None
if not tok:
    sys.exit("the token request answered: " + os.environ["RESP"][:300])
print(tok)'
}

# hw_get <token> <path> prints the body of a GET against the landscape, and
# fails on anything but a 200. The token reaches curl on stdin.
hw_get() {
  local out code
  out="$(curl_bearer "$1" -sS -m 30 -w '\n%{http_code}' "$HW_ENDPOINT$2")" || return 1
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  if [ "$code" != 200 ]; then
    printf 'GET %s answered %s: %s\n' "$2" "$code" "$(printf '%s' "$out" | head -c 300)" >&2
    return 1
  fi
  printf '%s' "$out"
}

# hw_find <body> <list key> [field=value...] prints the id of each row of the
# body's list whose fields all match, and fails when none does. A value that
# ends in * matches a prefix. true and false match JSON booleans.
hw_find() {
  BODY="$1" python3 - "${@:2}" <<'PY'
import json, os, sys
key, conds = sys.argv[1], [c.split("=", 1) for c in sys.argv[2:]]
def text(v):
    return str(v).lower() if isinstance(v, bool) else str(v)
def ok(row):
    for field, want in conds:
        have = text(row.get(field))
        if not (have.startswith(want[:-1]) if want.endswith("*") else have == want):
            return False
    return True
rows = [r for r in json.loads(os.environ["BODY"]).get(key) or [] if ok(r)]
for r in rows:
    print(r.get("id") or r.get("versionId"))
sys.exit(0 if rows else 1)
PY
}

# hw_on <switch> is true when this run turns <switch> on.
hw_on() { case " $HW_ON " in *" $1 "*) return 0 ;; esac; return 1; }

# hw_preflight reads the landscape and creates nothing, so a missing piece
# fails in seconds, not an hour into a create.
hw_preflight() {
  local token body left base="/v1/regions/$HW_REGION/projects/$HW_PROJECT"
  step "Preflight: read the landscape, create nothing"
  token="$(hw_mint)" || die "the credential got no token from $HW_ENDPOINT"
  good "the credential gets a token"
  body="$(hw_get "$token" /v1/regions)" || die "could not list the regions"
  hw_find "$body" regions "id=$HW_REGION" > /dev/null || die "the landscape serves no region $HW_REGION"
  good "region $HW_REGION"
  body="$(hw_get "$token" "$base/compute/clusters?limit=100")" \
    || die "could not list the clusters in project $HW_PROJECT. Check the project id, and that the credential has compute-admin there."
  good "project $HW_PROJECT"
  if left="$(hw_find "$body" clusters 'id=rapi-hw-*')"; then
    warn "clusters an earlier hardware run left in $HW_PROJECT: $(printf '%s' "$left" | tr '\n' ' ')"
  fi
  body="$(hw_get "$token" "/v1/regions/$HW_REGION/compute/cluster-types/$REAL_API_CLUSTER_TYPE/versions")" \
    || die "could not read cluster type $REAL_API_CLUSTER_TYPE's versions in $HW_REGION"
  hw_find "$body" versions status=available > /dev/null \
    || die "cluster type $REAL_API_CLUSTER_TYPE has no available version in $HW_REGION"
  good "cluster type $REAL_API_CLUSTER_TYPE has an available version"
  if hw_on network; then
    body="$(hw_get "$token" "/v1/regions/$HW_REGION/networking/address-pools")" || die "could not list the address pools"
    if [ -n "${REAL_API_ADDRESS_POOL:-}" ]; then
      hw_find "$body" addressPools "id=$REAL_API_ADDRESS_POOL" state=active > /dev/null \
        || die "no active address pool $REAL_API_ADDRESS_POOL in $HW_REGION"
      good "address pool $REAL_API_ADDRESS_POOL is active"
    else
      hw_find "$body" addressPools state=active routable=false > /dev/null \
        || die "no active address pool that isn't routable in $HW_REGION, for the subnets. Name one in REAL_API_ADDRESS_POOL."
      good "an active address pool that isn't routable"
    fi
  fi
  if hw_on file_store; then
    body="$(hw_get "$token" "/v1/regions/$HW_REGION/storage/share-types")" || die "could not list the share types"
    hw_find "$body" shareTypes state=active > /dev/null \
      || die "no active share type in $HW_REGION for the file store. Turn it off with REAL_API_BUILD_OFF=file_store, and the record says so."
    good "an active share type"
  fi
  result PASS preflight
}

# hw_holds <dir> is true when <dir>'s state still holds a managed resource,
# or when its state can't be read: a destroy of an empty state does nothing,
# and a skipped one can leave hardware running.
hw_holds() {
  local list
  [ -f "$1/terraform.tfstate" ] || return 1
  list="$(cd "$1" && "$TF_BIN" state list 2> /dev/null)" || return 0
  [ -n "$list" ] && printf '%s\n' "$list" | grep -qv '^data\.'
}

# hw_held <dir> prints each managed resource <dir>'s state holds, with its id.
hw_held() {
  ( cd "$1" && "$TF_BIN" show -json 2> /dev/null ) | python3 -c '
import json, sys
def walk(m):
    for r in m.get("resources", []):
        if r.get("mode") == "managed":
            print("       %s  id %s" % (r["address"], (r.get("values") or {}).get("id")))
    for c in m.get("child_modules", []):
        walk(c)
try:
    walk(json.load(sys.stdin).get("values", {}).get("root_module", {}))
except ValueError:
    pass'
}

# hw_env_for_destroy sets what a Terraform command against the landscape reads,
# without the recorder, which may be gone.
hw_env_for_destroy() {
  # shellcheck disable=SC2031 # cmd_run's subshell sets its own; this is hardware mode's
  export "${ENV_PREFIX}ENDPOINT=$HW_ENDPOINT" "${ENV_PREFIX}CLIENT_ID=$HW_CLIENT_ID" \
    "${ENV_PREFIX}CLIENT_SECRET=$HW_CLIENT_SECRET" TF_CLI_CONFIG_FILE="$HW_RUN/terraform.rc"
  unset "${ENV_PREFIX}REGION" "${ENV_PREFIX}PROJECT"
}

# hw_cleanup runs on any exit: it stops a running Terraform, destroys what the
# landscape side still holds unless REAL_API_KEEP=1, and prints the summary.
hw_cleanup() {
  local rc=$? d
  trap - EXIT INT TERM
  if [ -n "${TF_PID:-}" ] && kill -0 "$TF_PID" 2> /dev/null; then
    say "stopping Terraform in $TF_STEP before the cleanup. Ctrl-C again leaves everything in place."
    kill -INT "$TF_PID" 2> /dev/null
    wait "$TF_PID" 2> /dev/null
    # Terraform writes the state as it exits, and the check below reads it,
    # so the cleanup waits for the process itself to be gone.
    while kill -0 "$TF_PID" 2> /dev/null; do sleep 1; done
    result STOP "$TF_STEP" "interrupted"
  fi
  if [ "${REAL_API_KEEP:-0}" != 1 ]; then
    hw_env_for_destroy
    for d in "$HW_RUN/real/full-everything" "$HW_RUN/real/look"; do
      hw_holds "$d" || continue
      say "cleanup: destroying what ${d#"$HW_RUN"/} still holds. Log: $d-cleanup.log"
      if ( cd "$d" && "$TF_BIN" destroy -auto-approve -input=false -no-color ) > "$d-cleanup.log" 2>&1; then
        result PASS "cleanup ${d#"$HW_RUN"/}"
      else
        result FAIL "cleanup ${d#"$HW_RUN"/}" "$d-cleanup.log"
      fi
    done
  fi
  # Reaped here, bash doesn't print a "Terminated" line for each. A bare wait
  # would wait for every job, so it runs only with pids.
  if [ -n "${proxy_pid:-}${fake_pid:-}" ]; then
    # shellcheck disable=SC2086 # a list of pids
    kill ${proxy_pid:-} ${fake_pid:-} 2> /dev/null
    # shellcheck disable=SC2086
    wait ${proxy_pid:-} ${fake_pid:-} 2> /dev/null
  fi
  hw_summary "$rc"
  rm -f "$HW_LOCK/pid"
  rmdir "$HW_LOCK" 2> /dev/null
  exit "$rc"
}

# hw_summary <exit status> prints one screen: what ran, the drift result, the
# record, how long it took, and what's left on the landscape with the
# commands that remove it.
hw_summary() {
  local d held left any=0
  step "Summary: hardware run $HW_ID on $HW_LABEL, API $HW_API, $(took $((SECONDS - HW_START)))"
  grep -E '^(PASS|FAIL|STOP|SKIP) ' "$RESULTS" | sed 's/^/   /'
  [ -s "$HW_RUN/compare.txt" ] && note "fake drift: $(grep 'operations compared' "$HW_RUN/compare.txt")"
  note "$HW_RECORD_SAYS"
  for d in "$HW_RUN/real/full-everything" "$HW_RUN/real/look"; do
    hw_holds "$d" || continue
    held="$(hw_held "$d")"
    any=1
    warn "${d#"$HW_RUN"/} still holds, on $HW_HOST:"
    printf '%s\n' "$held"
    warn "remove it with your landscape's variables set, as for this run:"
    warn "  cd $d && TF_CLI_CONFIG_FILE=$HW_RUN/terraform.rc $TF_BIN destroy"
  done
  if grep -l 'TERRAFORM CRASH' "$HW_RUN"/real/*.log > /dev/null 2>&1; then
    any=1
    warn "Terraform crashed in $(grep -l 'TERRAFORM CRASH' "$HW_RUN"/real/*.log | tr '\n' ' '). Its state may miss what it made."
  fi
  # State can miss what the platform made, after a crash or a kill, so ask
  # the landscape too.
  left="$(hw_landscape_left)"
  if printf '%s' "$left" | grep -q '^       (could not'; then
    any=1
    warn "could not read $HW_HOST to check what's left there, outside state:"
    printf '%s\n' "$left" | grep '^       (could not'
  fi
  if printf '%s' "$left" | grep -qv '^       (could not'; then
    any=1
    warn "the landscape still has, named for this run:"
    printf '%s\n' "$left" | grep -v '^       (could not'
    warn "anything no state above holds has to go in the console or through the API"
  fi
  [ "$any" = 1 ] || note "nothing left on $HW_HOST from this run, in state or on the landscape"
  note "Results: $RESULTS"
  printf '%s\n' "${1:-0}" > "$HW_RUN/exit"
}

# hw_landscape_left prints each object in the project whose id starts with
# this run's id, as "<kind> <id>", read from the landscape, not from state.
hw_landscape_left() {
  local token body kind path base="/v1/regions/$HW_REGION/projects/$HW_PROJECT"
  token="$(hw_mint 2> /dev/null)" || { echo "       (could not read the landscape: no token)"; return 0; }
  for kind in compute/clusters:clusters compute/instance-groups:instanceGroups storage/file-stores:fileStores \
      networking/networks:networks networking/security-groups:securityGroups; do
    path="${kind%%:*}"
    body="$(hw_get "$token" "$base/$path?limit=100" 2> /dev/null)" || { echo "       (could not list $path)"; continue; }
    hw_find "$body" "${kind#*:}" "id=$HW_ID*" | sed "s|^|       ${path#*/} |"
  done
}

# hw_covered prints each resource type the landscape created, from the real
# side's apply log.
hw_covered() {
  sed -n 's/^\(k0rdent_[a-z_]*\)\.[^:]*: Creation complete.*/\1/p' "$HW_RUN/real/full-everything-apply.log" 2> /dev/null | sort -u
}

cmd_hardware() {
  local fake code want s fake_url work
  need go; need python3; need ssh-keygen; need curl
  read_names
  find_terraform
  HW_ENDPOINT="$(eval printf '%s' "\"\${${ENV_PREFIX}ENDPOINT:-}\"")"
  [ -n "$HW_ENDPOINT" ] || die "${ENV_PREFIX}ENDPOINT is not set. It is the landscape's gateway, such as https://api.example.com"
  HW_HOST="$(endpoint_host "$HW_ENDPOINT")"
  case "$HW_HOST" in
    localhost | localhost:* | 127.* | \[::1\]*)
      # The script test stands a fake in for a landscape on loopback.
      [ "${REAL_API_HARDWARE_TEST_LOOPBACK:-}" = 1 ] \
        || die "${ENV_PREFIX}ENDPOINT is $HW_ENDPOINT, on this machine. Hardware mode runs against a landscape with hardware. For a kind stack, run make real-api-check." ;;
  esac
  endpoint_is_encrypted "$HW_ENDPOINT" \
    || die "${ENV_PREFIX}ENDPOINT is $HW_ENDPOINT. Use https: the credential goes in the body of the token request."
  [ -n "${REAL_API_CONFIRM:-}" ] || die "make real-api-check-hardware creates real clusters and hosts on $HW_HOST, for hours.

   Confirm the landscape by naming its host:

       REAL_API_CONFIRM=$HW_HOST make real-api-check-hardware"
  [ "$REAL_API_CONFIRM" = "$HW_HOST" ] \
    || die "REAL_API_CONFIRM is \"$REAL_API_CONFIRM\", and ${ENV_PREFIX}ENDPOINT's host is \"$HW_HOST\". They have to match."
  for s in REGION PROJECT CLIENT_ID CLIENT_SECRET; do
    [ -n "$(eval printf '%s' "\"\${${ENV_PREFIX}$s:-}\"")" ] \
      || die "${ENV_PREFIX}$s is not set. TESTING.md lists what make real-api-check-hardware needs."
  done
  HW_REGION="$(eval printf '%s' "\"\${${ENV_PREFIX}REGION}\"")"
  HW_PROJECT="$(eval printf '%s' "\"\${${ENV_PREFIX}PROJECT}\"")"
  HW_CLIENT_ID="$(eval printf '%s' "\"\${${ENV_PREFIX}CLIENT_ID}\"")"
  HW_CLIENT_SECRET="$(eval printf '%s' "\"\${${ENV_PREFIX}CLIENT_SECRET}\"")"
  [ -n "${REAL_API_CLUSTER_TYPE:-}" ] \
    || die "REAL_API_CLUSTER_TYPE is not set. It is a cluster type id in $HW_REGION with a worker pool and hardware free for a control plane, its workers and one host."
  HW_ON=""
  for s in ${REAL_API_BUILD_OFF:-}; do
    case " $HW_BUILD_ON " in *" $s "*) ;; *) die "REAL_API_BUILD_OFF names $s. The switches this run turns on are: $HW_BUILD_ON" ;; esac
  done
  for s in $HW_BUILD_ON; do
    case " ${REAL_API_BUILD_OFF:-} " in *" $s "*) ;; *) HW_ON="$HW_ON $s" ;; esac
  done
  # Terraform 1.16.0 to 1.16.4 can crash on one interrupt while two creates
  # wait, and empty the state file (hashicorp/terraform#39287, fixed in
  # 1.16.5). This run's cleanup interrupts Terraform, and the state is all
  # that says which hosts it made.
  case "$("$TF_BIN" version 2> /dev/null | head -1)" in
    "Terraform v1.16."[0-4])
      die "$("$TF_BIN" version | head -1) can lose the whole state when a run is interrupted, and this run's cleanup interrupts it. Use Terraform 1.16.5 or later, or OpenTofu, with TF_BIN=<path>." ;;
  esac
  HW_LABEL="${REAL_API_LANDSCAPE:-hardware}"
  printf '%s' "$HW_LABEL" | grep -Eq '^[a-z][a-z0-9-]{0,30}$' \
    || die "REAL_API_LANDSCAPE is \"$HW_LABEL\". It goes in a public record, so it's a short label such as lab-a, never a host name: lower-case letters, digits and hyphens."
  hw_api_version || exit 1
  want="$(want_api_version "$REPO")" || exit 1
  fake="$(tree_hash fake "$REPO")" || die "could not hash the tree"
  code="$(tree_hash code "$REPO")" || die "could not hash the tree"

  HW_START=$SECONDS
  HW_ID="rapi-hw-$(date +%s | cut -c5-)"
  HW_RUN="$REPO/.real-api/hardware/$HW_ID"
  private_dir "$REPO/.real-api"
  mkdir -p "$HW_RUN" || die "could not make $HW_RUN"
  HW_LOCK="$REPO/.real-api/hardware/$HW_HOST.lock"
  mkdir "$HW_LOCK" 2> /dev/null \
    || die "another hardware run against $HW_HOST holds $HW_LOCK (pid $(cat "$HW_LOCK/pid" 2> /dev/null)). If none is running, remove the directory."
  echo $$ > "$HW_LOCK/pid"
  RESULTS="$HW_RUN/results.txt"
  HW_RECORD_SAYS="no record written: the run didn't finish"
  {
    printf 'k0rdent AI Terraform provider: real-API check on hardware\n'
    printf 'run:          %s\n' "$HW_ID"
    printf 'date:         %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf 'landscape:    %s (%s)\n' "$HW_LABEL" "$HW_HOST"
    printf 'api version:  %s, %s. The provider tracks %s\n' "$HW_API" "$HW_API_SOURCE" "$want"
    printf 'where:        region %s, project %s, cluster type %s\n' "$HW_REGION" "$HW_PROJECT" "$REAL_API_CLUSTER_TYPE"
    printf 'switches on: %s\n' "$HW_ON"
    printf '\n'
  } > "$RESULTS"

  printf '%sk0rdent AI Terraform provider: real-API check on hardware%s\n' "$B" "$R"
  warn "this creates real clusters and hosts on $HW_HOST and destroys them again. It takes hours."
  note "run $HW_ID, results in $RESULTS"
  [ "$HW_API" = "$want" ] || warn "the landscape runs API $HW_API, and ci/real-api/api-version says $want. The record says both."
  # Everything below runs in this shell, not a subshell, so a signal reaches
  # hw_cleanup at once and it can stop the running Terraform.
  trap hw_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  hw_preflight

  work="$HW_RUN"
  step "Build"
  build_provider "$work" 0.0.0-real-api
  export TF_CLI_CONFIG_FILE="$work/terraform.rc" TF_IN_AUTOMATION=1
  (cd "$REPO" && go build -o "$work/fakeapi" ./internal/tools/fakeapi && go build -o "$work/fakedrift" ./internal/tools/fakedrift) \
    || die "could not build the fake or fakedrift"
  result PASS build

  HEARTBEAT="${REAL_API_HEARTBEAT:-300}"
  step "The fake: look/ and full-everything"
  start_loopback_server "$work/fakeapi" -secrets-file "$work/fake-secrets.env" "$work/fake.url" "$work/fake.log" \
    || die "the fake did not start: $(cat "$work/fake.log")"
  fake_pid="$SERVER_PID"
  fake_url="$SERVER_URL"
  recorder "$fake_url" "$work/fake.ndjson"
  export_fake_credential "$work" "$work/fake-secrets.env"
  export "${ENV_PREFIX}ENDPOINT=$recorder"
  unset "${ENV_PREFIX}REGION" "${ENV_PREFIX}PROJECT"
  walkthroughs "$work/fake" hw-fake

  step "The landscape: look/ and full-everything on $HW_HOST"
  recorder "$HW_ENDPOINT" "$work/real.ndjson"
  # As a reader runs the walkthrough: the endpoint and the credential only.
  export "${ENV_PREFIX}ENDPOINT=$recorder" "${ENV_PREFIX}CLIENT_ID=$HW_CLIENT_ID" "${ENV_PREFIX}CLIENT_SECRET=$HW_CLIENT_SECRET"
  PACE="${REAL_API_MINT_PACE:-7}" walkthroughs "$work/real" hw-real

  step "Fake drift: compare"
  # -partial: this run covers a slice of the operations, so an accepted line it
  # didn't see is listed, not failed. The run on kind keeps the list current.
  if "$work/fakedrift" compare -partial -spec "$REPO/internal/k0rdent/spec/openapi.yaml" \
      -fake "$work/fake.ndjson" -real "$work/real.ndjson" -accepted "$ACCEPTED" > "$work/compare.txt"; then
    good "$(grep 'operations compared' "$work/compare.txt")"
    result PASS "fake drift" "$(grep 'operations compared' "$work/compare.txt")"
  else
    cat "$work/compare.txt"
    warn "fix the fake to answer as the real API does, or add a line with the reason to ci/fake-drift-accepted.json"
    result FAIL "fake drift" "$work/compare.txt"
  fi

  if [ "${REAL_API_KEEP:-0}" = 1 ]; then
    HW_RECORD_SAYS="no record written: REAL_API_KEEP=1 skips the destroys, so the run isn't whole"
    exit 1
  fi
  if grep -q '^FAIL' "$RESULTS" || awk '$2 != 0 {bad = 1} END {exit !bad}' "$work/fake/steps" "$work/real/steps"; then
    HW_RECORD_SAYS="no record written: a step failed. ci/real-api/hardware-record.json is unchanged"
    exit 1
  fi
  RECORD="$HW_RECORD" LABEL="$HW_LABEL" API="$HW_API" SOURCE="$HW_API_SOURCE" TRACKS="$want" FAKE="$fake" CODE="$code" \
    ON="$HW_ON" KIND_COVERS="$HW_KIND_COVERS" OFF="${REAL_API_BUILD_OFF:-}" COVERED="$(hw_covered)" \
    TOOK="$(took $((SECONDS - HW_START)))" TF="$("$TF_BIN" version | head -1)" python3 -c '
import json, os, datetime
e = os.environ
build = {s: "on" for s in e["ON"].split()}
build.update({s: "off, kind covers it" for s in e["KIND_COVERS"].split()})
build.update({s: "off for this run" for s in e["OFF"].split()})
json.dump({
    "landscape": e["LABEL"],
    "api_version": e["API"],
    "api_version_source": e["SOURCE"],
    "provider_tracks": e["TRACKS"],
    "fake": e["FAKE"],
    "code": e["CODE"],
    "terraform": e["TF"],
    "steps": {"walkthroughs": "PASS", "fake drift": "PASS"},
    "build": build,
    "covered": e["COVERED"].split(),
    "ran": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "took": e["TOOK"],
}, open(e["RECORD"], "w"), indent=2)
open(e["RECORD"], "a").write("\n")' || die "could not write $HW_RECORD"
  HW_RECORD_SAYS="wrote ci/real-api/hardware-record.json. Commit it."
  result PASS record "covers: $(hw_covered | tr '\n' ' ')"
}

case "${1:-}" in
  run) cmd_run ;;
  hardware) cmd_hardware ;;
  verify) check_record "$REPO" fake ;;
  verify-release) check_record "${2:-$REPO}" code ;;
  hash) cmd_hash "${2:-}" "${3:-}" ;;
  *) die "usage: scripts/real-api-check.sh run|hardware|verify|verify-release [TREE]|hash fake|code|fake-list|code-list|hashing [TREE]" ;;
esac
