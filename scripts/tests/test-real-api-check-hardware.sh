#!/usr/bin/env bash
#
# Runs scripts/real-api-check.sh hardware against a fake standing in for a
# landscape, in a copy of this tree, so nothing here needs hardware:
#
#   * a run that passes writes ci/real-api/hardware-record.json, naming the
#     resources it created, and prints a line while Terraform waits.
#   * a run killed while it creates stops Terraform, destroys what it made,
#     and leaves results.txt saying so.
#   * a run whose landscape goes away while it creates lists what it left,
#     with each id and the command that removes it.
#
# The stand-in fake is on loopback, so the run sets
# REAL_API_HARDWARE_TEST_LOOPBACK, which only this test uses.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
land_pid=""
# REAL_API_TEST_KEEP=1 keeps the work directory, for a failure's logs.
trap '[ -z "$land_pid" ] || { kill "$land_pid"; wait "$land_pid"; } 2>/dev/null; [ "${REAL_API_TEST_KEEP:-}" = 1 ] && echo "kept $work" >&2 || rm -rf "$work"' EXIT

fail() { echo "real-api-check hardware test: FAIL - $1" >&2; exit 1; }
has() { grep -q -- "$2" "$1" || { cat "$1" >&2; fail "no \"$2\" in $1"; }; }

# A copy of the tree as it is, committed or not, so the run's record and
# .real-api/ land there.
repo="$work/repo"
mkdir -p "$repo"
git -C "$here" ls-files -z --cached --others --exclude-standard \
  | (cd "$here" && tar --null -T - -cf -) | tar -xf - -C "$repo" || fail "could not copy the tree"
git -C "$repo" init -q && git -C "$repo" add -A || fail "could not make the copy a repository"
rm -f "$repo/ci/real-api/hardware-record.json"
(cd "$repo" && go build -o "$work/fakeapi" ./internal/tools/fakeapi) || fail "could not build the fake"

# start_landscape <latency> starts the stand-in and sets land_pid and LAND.
start_landscape() {
  rm -f "$work/land.env"
  "$work/fakeapi" -secrets-file "$work/land.env" -latency "$1" > "$work/land.url" 2> "$work/land.log" &
  land_pid=$!
  for _ in $(seq 1 100); do [ -s "$work/land.url" ] && [ -s "$work/land.env" ] && break; sleep 0.1; done
  LAND="$(cat "$work/land.url")"
  [ -n "$LAND" ] || fail "the stand-in fake did not start: $(cat "$work/land.log")"
  # shellcheck source=/dev/null # written by fakeapi
  . "$work/land.env"
}

# hardware <output file> runs the check against the stand-in. It execs, so
# call it in a subshell: then a background one's $! is the check itself, and
# a signal sent to it reaches the check's trap.
hardware() {
  exec env K0RDENT_ENDPOINT="$LAND" REAL_API_CONFIRM="${LAND#http://}" REAL_API_HARDWARE_TEST_LOOPBACK=1 \
    K0RDENT_REGION=us-texas-1 K0RDENT_PROJECT=demo K0RDENT_CLIENT_ID="$FAKE_CLIENT_ID" \
    K0RDENT_CLIENT_SECRET="$FAKE_CLIENT_SECRET" REAL_API_CLUSTER_TYPE=gpu-training \
    REAL_API_VERSION=v1.3.0-rc.5 REAL_API_LANDSCAPE=stand-in REAL_API_HEARTBEAT=5 REAL_API_MINT_PACE=0 \
    "$repo/scripts/real-api-check.sh" hardware > "$1" 2>&1
}

# the_run prints the run directory the output names.
the_run() { sed -n 's|.*results in \(.*\)/results.txt$|\1|p' "$1" | head -1; }

# wait_for <file> <text>: until <text> is in <file>, for up to five minutes.
wait_for() {
  for _ in $(seq 1 300); do grep -qs -- "$2" "$1" && return 0; sleep 1; done
  cat "$1" >&2
  fail "the run never printed \"$2\""
}

# mid_create <output>: until the landscape side's apply has created its first
# resource. The others take about a minute on the stand-in, so a kill or a
# lost landscape then lands with something in state and creates in flight,
# however slow the machine is.
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

# --- a run that passes writes the record --------------------------------------

start_landscape 50ms
out="$work/pass.out"
( hardware "$out" ) || { cat "$out" >&2; fail "a run against the stand-in exited non-zero"; }
run="$(the_run "$out")"
for step in preflight build real/look real/full-everything-apply real/full-everything-replan \
    real/full-everything-destroy real/look-destroy "fake drift" record; do
  grep -qE "^PASS +$step( |$)" "$run/results.txt" || { cat "$run/results.txt" >&2; fail "no PASS line for $step"; }
done
has "$out" "real/full-everything-apply: [0-9]*[sm] so far, still creating k0rdent_"
has "$out" "nothing left on ${LAND#http://} from this run"
[ -z "$(left_on_standin "$(basename "$run")")" ] || fail "a run that passed left $(left_on_standin "$(basename "$run")")"
record="$repo/ci/real-api/hardware-record.json"
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
rm -f "$record"

# --- a run killed while it creates cleans up ----------------------------------

kill "$land_pid"; wait "$land_pid" 2>/dev/null
start_landscape 400ms
out="$work/kill.out"
( hardware "$out" ) &
check_pid=$!
mid_create "$out"
kill -TERM "$check_pid"
wait "$check_pid"
rc=$?
[ "$rc" = 143 ] || { cat "$out" >&2; fail "a killed run exited $rc, want 143"; }
run="$(the_run "$out")"
has "$run/results.txt" "^STOP  real/full-everything-apply"
has "$run/results.txt" "^PASS  cleanup real/full-everything"
says_what_is_left "$out" "$run"
[ ! -f "$record" ] || fail "a killed run wrote a record"
[ ! -d "$repo/.real-api/hardware/${LAND#http://}.lock" ] || fail "a killed run left its lock"

# --- ctrl-C stops Terraform with one interrupt, not two -----------------------

# A terminal's ctrl-C signals the whole foreground process group. The run gets
# its own group here, as a terminal would give it, and the INT goes to all of
# it. Two interrupts make Terraform exit at once, which can lose the state of
# a resource it's creating.
out="$work/ctrl-c.out"
# A job started with & has SIGINT ignored, and bash can't trap a signal
# ignored on entry, so the wrapper sets it back, as a terminal would have it.
python3 -c 'import os, signal, sys; os.setpgrp(); signal.signal(signal.SIGINT, signal.SIG_DFL); os.execvp(sys.argv[1], sys.argv[1:])' \
  bash -c "$(declare -f hardware); LAND='$LAND' FAKE_CLIENT_ID='$FAKE_CLIENT_ID' FAKE_CLIENT_SECRET='$FAKE_CLIENT_SECRET' repo='$repo' hardware '$out'" &
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
has "$run/results.txt" "^PASS  cleanup real/full-everything"
says_what_is_left "$out" "$run"

# --- a run whose landscape goes away lists what it left -----------------------

out="$work/gone.out"
( hardware "$out" ) &
check_pid=$!
mid_create "$out"
kill "$land_pid"; wait "$land_pid" 2>/dev/null; land_pid=""
wait "$check_pid"
rc=$?
[ "$rc" != 0 ] || { cat "$out" >&2; fail "a run whose landscape went away passed"; }
run="$(the_run "$out")"
has "$out" "real/full-everything still holds, on ${LAND#http://}:"
has "$out" "k0rdent_[a-z_]*\.this[^ ]*  id "
has "$out" "cd $run/real/full-everything && TF_CLI_CONFIG_FILE=$run/terraform.rc .* destroy"
# The apply fails once the landscape is gone, and the destroy after it fails
# too: as the run's own step, or in the cleanup when the run ends first.
has "$run/results.txt" "^FAIL  \(cleanup real/full-everything\|real/full-everything-destroy\)"
has "$out" "could not read ${LAND#http://} to check what's left there, outside state"

echo "real-api-check hardware test: ok"
