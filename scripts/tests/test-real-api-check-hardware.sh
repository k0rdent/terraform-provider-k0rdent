#!/usr/bin/env bash
#
# Runs scripts/real-api-check.sh hardware against a fake standing in for a
# landscape, in a copy of this tree, so nothing here needs hardware:
#
#   * a run that passes writes ci/real-api/hardware-record.json, naming the
#     resources it created, and prints a line while Terraform waits.
#   * a run killed while it creates stops Terraform, destroys what it made,
#     and leaves results.txt saying so.
#   * a run stopped with ctrl-C does the same, with one interrupt to
#     Terraform, and says what's left.
#   * a run whose landscape goes away while it creates lists what it left,
#     with each id and the command that removes it.
#
# The four runs go at once, each in its own copy of the tree against its own
# stand-in. The provider polls every 5s, so a create in the run that passes
# outlasts the 10s Terraform waits before it says "Still creating", and a kill
# has a poll to land in, as mid_create says. The run whose landscape goes away
# polls every second.
#
# The stand-in fake is on loopback, so the run sets
# REAL_API_HARDWARE_TEST_LOOPBACK, which only this test uses.
set -uo pipefail

# BASHPID, below, needs bash 4. macOS's /bin/bash is 3.2.
if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  echo "real-api-check hardware test: needs bash 4 or later, this is $BASH_VERSION. On macOS, put Homebrew's bash first on PATH" >&2
  exit 1
fi

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
pids=()

# below <pid> prints the pid of every process under <pid>. Terraform runs in
# a process group of its own, so a group's kill misses it.
below() {
  local c
  for c in $(pgrep -P "$1"); do echo "$c"; below "$c"; done
}

# stop_all stops the runs still going, with every process under them, their
# stand-ins included. On a failure it prints the end of each run's output.
# REAL_API_TEST_KEEP=1 keeps the work directory, for a failure's logs.
stop_all() {
  local rc=$? pid out
  for pid in "${pids[@]}"; do
    # shellcheck disable=SC2046 # a list of pids
    [ -z "$pid" ] || kill -KILL $(below "$pid") "$pid" 2> /dev/null
  done
  wait 2> /dev/null
  if [ "$rc" != 0 ]; then
    for out in "$work"/*/out; do
      [ -f "$out" ] || continue
      echo "--- the end of the $(basename "${out%/out}") run's output:" >&2
      tail -n 20 "$out" >&2
    done
  fi
  [ "${REAL_API_TEST_KEEP:-}" = 1 ] && echo "kept $work" >&2 || rm -rf "$work"
}
trap stop_all EXIT
# In an EXIT trap a signal leaves $? at 0, so these give stop_all the
# failure to print each run's output for.
trap 'exit 143' TERM
trap 'exit 130' INT

scenario=setup
fail() { echo "real-api-check hardware test: FAIL - $scenario: $1" >&2; exit 1; }
has() { grep -q -- "$2" "$1" || { cat "$1" >&2; fail "no \"$2\" in $1"; }; }

# A copy of the tree as it is, committed or not, so the run's record and
# .real-api/ land there. Each run gets its own copy of it.
tree="$work/tree"
mkdir -p "$tree"
git -C "$here" ls-files -z --cached --others --exclude-standard \
  | (cd "$here" && tar --null -T - -cf -) | tar -xf - -C "$tree" || fail "could not copy the tree"
git -C "$tree" init -q && git -C "$tree" add -A || fail "could not make the copy a repository"
rm -f "$tree/ci/real-api/hardware-record.json"
(cd "$tree" && go build -o "$work/fakeapi" ./internal/tools/fakeapi) || fail "could not build the fake"
# Compiled once here, so the four runs' own builds compile only this
# module's packages.
(cd "$tree" && go build . ./internal/tools/fakedrift ./internal/tools/nameaudit) || fail "could not build the provider"

# start_landscape <latency> starts the run's stand-in in $dir and sets
# land_pid and LAND.
start_landscape() {
  "$work/fakeapi" -secrets-file "$dir/land.env" -latency "$1" > "$dir/land.url" 2> "$dir/land.log" &
  land_pid=$!
  for _ in $(seq 1 100); do [ -s "$dir/land.url" ] && [ -s "$dir/land.env" ] && break; sleep 0.1; done
  LAND="$(cat "$dir/land.url")"
  [ -n "$LAND" ] || fail "the stand-in fake did not start: $(cat "$dir/land.log")"
  # shellcheck source=/dev/null # written by fakeapi
  . "$dir/land.env"
}

# scenario <name> <latency> <poll>: a run's own directory, copy of the tree
# and stand-in. Call it in the subshell the run goes in: when that ends,
# everything it started stops, the check and Terraform included.
scenario() {
  # shellcheck disable=SC2064 # the subshell's pid, now
  trap "kill -KILL \$(below $BASHPID) 2> /dev/null" EXIT
  scenario="$1" dir="$work/$1" poll="$3"
  repo="$dir/repo"
  record="$repo/ci/real-api/hardware-record.json"
  mkdir -p "$dir" && cp -R "$tree" "$repo" || fail "could not copy the tree"
  start_landscape "$2"
}

# hardware <output file> runs the check against the stand-in. It execs, so
# call it in a subshell: then a background one's $! is the check itself, and
# a signal sent to it reaches the check's trap.
hardware() {
  exec env K0RDENT_ENDPOINT="$LAND" REAL_API_CONFIRM="${LAND#http://}" REAL_API_HARDWARE_TEST_LOOPBACK=1 \
    K0RDENT_REGION=us-texas-1 K0RDENT_PROJECT=demo K0RDENT_CLIENT_ID="$FAKE_CLIENT_ID" \
    K0RDENT_CLIENT_SECRET="$FAKE_CLIENT_SECRET" K0RDENT_POLL_INTERVAL="$poll" REAL_API_CLUSTER_TYPE=gpu-training \
    REAL_API_VERSION=v1.3.0-rc.5 REAL_API_LANDSCAPE=stand-in REAL_API_HEARTBEAT=5 REAL_API_MINT_PACE=0 \
    "$repo/scripts/real-api-check.sh" hardware > "$1" 2>&1
}

# the_run prints the run directory the output names.
the_run() { sed -n 's|.*results in \(.*\)/results.txt$|\1|p' "$1" | head -1; }

# wait_for <file> <text>: until <text> is in <file>, for up to five minutes,
# or until the check in $check_pid exits without printing it.
wait_for() {
  for _ in $(seq 1 1500); do
    grep -qs -- "$2" "$1" && return 0
    if [ -n "${check_pid:-}" ] && ! kill -0 "$check_pid" 2> /dev/null; then
      grep -qs -- "$2" "$1" && return 0
      cat "$1" >&2
      fail "the check exited before it printed \"$2\""
    fi
    sleep 0.2
  done
  cat "$1" >&2
  fail "the run never printed \"$2\""
}

# mid_create <output>: until the landscape side's apply has created its first
# resource, the instance group. The network and security group take one more
# 5s poll, and the subnets and the cluster start after them, so a kill then
# lands with something in state and creates in flight. A kill after those
# start can't be cleaned up: the stand-in answers a delete of a subnet still
# `creating` with a 409, and the network and security group are in use. A
# kill up to 4s late still passed, 5s late failed.
mid_create() {
  wait_for "$1" "real/full-everything-apply started"
  wait_for "$(the_run "$1")/real/full-everything-apply.log" "Creation complete"
}

# left_on_standin <run id> prints "<kind> <id>" for each object the stand-in
# still holds whose id starts with <run id>, the way the summary names them.
left_on_standin() {
  local tok kind
  tok="$(curl -sS -m 10 -X POST "$LAND/v1/regions/global/auth/token" -H 'Content-Type: application/json' \
    --data "{\"grant_type\":\"client_credentials\",\"client_id\":\"$FAKE_CLIENT_ID\",\"client_secret\":\"$FAKE_CLIENT_SECRET\"}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')" || fail "no token from the stand-in"
  for kind in compute/clusters:clusters compute/instance-groups:instanceGroups storage/file-stores:fileStores \
      networking/networks:networks networking/security-groups:securityGroups; do
    printf 'Authorization: Bearer %s\n' "$tok" | curl -sS -m 10 -H @- "$LAND/v1/regions/us-texas-1/projects/demo/${kind%%:*}?limit=100" \
      | python3 -c 'import json,sys; [print(sys.argv[1], o["id"]) for o in json.load(sys.stdin).get(sys.argv[2]) or [] if o["id"].startswith(sys.argv[3])]' \
        "${kind#*/}" "${kind#*:}" "$1" | sed 's/:[^ ]* / /'
  done
}

# says_what_is_left <output> <run dir>: the summary names every object the
# stand-in still holds for the run, and says nothing is left only when that's
# so. A create that a kill interrupts with its request in flight leaves an
# object no state holds when its id is set in configuration, as the
# walkthrough's file store is, so a killed run may leave one.
says_what_is_left() {
  local left kind id
  left="$(left_on_standin "$(basename "$2")")"
  if [ -z "$left" ]; then
    has "$1" "nothing left on ${LAND#http://} from this run"
    return
  fi
  has "$1" "the landscape still has, named for this run:"
  while read -r kind id; do
    grep -q "^ *$kind $id\$" "$1" || { cat "$1" >&2; fail "the stand-in still holds $kind $id, and the summary doesn't say so"; }
  done <<< "$left"
}

# unlocked: the run took away its lock on the stand-in.
unlocked() {
  [ ! -d "$repo/.real-api/hardware/${LAND#http://}.lock" ] || fail "the run left its lock"
}

# cleaned_up <output> <run dir>: the cleanup passed and left nothing in state.
cleaned_up() {
  has "$2/results.txt" "^PASS  cleanup real/full-everything"
  if grep -q " still holds, on " "$1"; then cat "$1" >&2; fail "the cleanup passed, and state still holds something"; fi
}

# --- a run that passes writes the record --------------------------------------

passes() {
  scenario pass 50ms 5s
  out="$dir/out"
  ( hardware "$out" ) || { cat "$out" >&2; fail "a run against the stand-in exited non-zero"; }
  run="$(the_run "$out")"
  for step in preflight build real/look real/full-everything-apply real/full-everything-replan \
      real/full-everything-destroy real/look-destroy "fake drift" record; do
    grep -qE "^PASS +$step( |$)" "$run/results.txt" || { cat "$run/results.txt" >&2; fail "no PASS line for $step"; }
  done
  has "$out" "real/full-everything-apply: [0-9]*[sm] so far, still creating k0rdent_"
  has "$out" "nothing left on ${LAND#http://} from this run"
  [ -z "$(left_on_standin "$(basename "$run")")" ] || fail "a run that passed left $(left_on_standin "$(basename "$run")")"
  [ -f "$record" ] || fail "a run that passed wrote no record"
  RECORD="$record" python3 -c '
import json, os, sys
r = json.load(open(os.environ["RECORD"]))
want = {"k0rdent_kubernetes_cluster", "k0rdent_subnet", "k0rdent_file_store", "k0rdent_instance_group", "k0rdent_vpc_peering"}
problems = []
if not want <= set(r["covered"]):
    problems.append("covered %s, missing %s" % (r["covered"], sorted(want - set(r["covered"]))))
if r["landscape"] != "stand-in" or r["api_version"] != "v1.3.0-rc.5" or r["api_version_source"] != "stated by the runner":
    problems.append("landscape or API version wrong: %s" % r)
if r["build"].get("webhook") != "off, kind covers it" or r["build"].get("file_store") != "on":
    problems.append("build switches wrong: %s" % r["build"])
if "127.0.0.1" in json.dumps(r):
    problems.append("the record names the host")
if problems:
    sys.exit("; ".join(problems))' || fail "the record is wrong"
  unlocked
}

# --- a run killed while it creates cleans up ----------------------------------

killed() {
  scenario kill 400ms 5s
  out="$dir/out"
  ( hardware "$out" ) &
  check_pid=$!
  mid_create "$out"
  kill -TERM "$check_pid"
  wait "$check_pid"
  rc=$?
  [ "$rc" = 143 ] || { cat "$out" >&2; fail "a killed run exited $rc, want 143"; }
  run="$(the_run "$out")"
  has "$run/results.txt" "^STOP  real/full-everything-apply"
  has "$run/real/full-everything-apply.log" "Interrupt received"
  cleaned_up "$out" "$run"
  says_what_is_left "$out" "$run"
  [ ! -f "$record" ] || fail "a killed run wrote a record"
  unlocked
}

# --- ctrl-C stops Terraform with one interrupt, not two -----------------------

# A terminal's ctrl-C signals the whole foreground process group. The run gets
# its own group here, as a terminal would give it, and the INT goes to all of
# it. Two interrupts make Terraform exit at once, which can lose the state of
# a resource it's creating.
ctrl_c() {
  scenario ctrl-c 400ms 5s
  out="$dir/out"
  # A job started with & has SIGINT ignored, and bash can't trap a signal
  # ignored on entry, so the wrapper sets it back, as a terminal would have it.
  python3 -c 'import os, signal, sys; os.setpgrp(); signal.signal(signal.SIGINT, signal.SIG_DFL); os.execvp(sys.argv[1], sys.argv[1:])' \
    bash -c "$(declare -f hardware); LAND='$LAND' FAKE_CLIENT_ID='$FAKE_CLIENT_ID' FAKE_CLIENT_SECRET='$FAKE_CLIENT_SECRET' poll='$poll' repo='$repo' hardware '$out'" &
  check_pid=$!
  mid_create "$out"
  kill -INT -- "-$check_pid"
  wait "$check_pid"
  rc=$?
  [ "$rc" = 130 ] || { cat "$out" >&2; fail "a run stopped with ctrl-C exited $rc, want 130"; }
  run="$(the_run "$out")"
  if grep -q 'Two interrupts received' "$run/real/full-everything-apply.log"; then
    cat "$run/real/full-everything-apply.log" >&2
    fail "ctrl-C reached Terraform twice"
  fi
  has "$run/real/full-everything-apply.log" "Interrupt received"
  cleaned_up "$out" "$run"
  says_what_is_left "$out" "$run"
  unlocked
}

# --- a run whose landscape goes away lists what it left -----------------------

gone() {
  scenario gone 400ms 1s
  out="$dir/out"
  ( hardware "$out" ) &
  check_pid=$!
  mid_create "$out"
  kill "$land_pid"; wait "$land_pid" 2>/dev/null
  wait "$check_pid"
  rc=$?
  [ "$rc" != 0 ] || { cat "$out" >&2; fail "a run whose landscape went away passed"; }
  run="$(the_run "$out")"
  has "$out" "real/full-everything still holds, on ${LAND#http://}:"
  held="$(cd "$run/real/full-everything" && "${TF_BIN:-terraform}" state list | grep -v '^data\.')"
  [ -n "$held" ] || fail "the run's state holds nothing, so the summary has nothing to list"
  while read -r a; do
    grep -qF "       $a  id " "$out" || { cat "$out" >&2; fail "state holds $a, and the summary doesn't say so"; }
  done <<< "$held"
  has "$out" "cd $run/real/full-everything && TF_CLI_CONFIG_FILE=$run/terraform.rc .* destroy"
  # The apply fails once the landscape is gone, and the destroy after it fails
  # too: as the run's own step, or in the cleanup when the run ends first.
  has "$run/results.txt" "^FAIL  \(cleanup real/full-everything\|real/full-everything-destroy\)"
  has "$out" "could not read ${LAND#http://} to check what's left there, outside state"
  unlocked
}

# --- all four at once ---------------------------------------------------------

# Each run prints a line as it ends. One still going after ten minutes,
# inside CI's fifteen, fails the test.
runs=(passes killed ctrl_c gone)
for r in "${runs[@]}"; do
  ( "$r" ) 2> "$work/$r.err" &
  pids+=($!)
done
start=$SECONDS failed=0 going=${#runs[@]}
while [ "$going" -gt 0 ]; do
  for i in "${!runs[@]}"; do
    [ -n "${pids[$i]}" ] && ! kill -0 "${pids[$i]}" 2> /dev/null || continue
    if wait "${pids[$i]}"; then
      echo "real-api-check hardware test: ${runs[$i]} passed after $((SECONDS - start))s"
    else
      failed=1
      cat "$work/${runs[$i]}.err" >&2
    fi
    pids[i]="" going=$((going - 1))
  done
  if [ "$going" -gt 0 ] && [ $((SECONDS - start)) -ge 600 ]; then
    for i in "${!runs[@]}"; do
      [ -z "${pids[$i]}" ] || echo "real-api-check hardware test: FAIL - ${runs[$i]} still going after 10 minutes" >&2
    done
    exit 1
  fi
  sleep 1
done
[ "$failed" = 0 ] || exit 1
echo "real-api-check hardware test: ok"
