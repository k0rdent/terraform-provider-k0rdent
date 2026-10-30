#!/usr/bin/env bash
#
# State written by the previous published release, installed from the public
# registry, must plan empty under the provider built from this tree, with the
# previous release's configuration. Runs over examples/walkthrough against
# the fake, under terraform and tofu.
#
# For each CLI and each directory the previous release's walkthrough has and
# the fake can apply:
#   1. Copy examples/walkthrough/<dir> from tag v$PREVIOUS, pin
#      version = "$PREVIOUS", init from the registry, apply, and plan again:
#      the previous release must plan empty on its own state. Its plan's
#      drift is kept, since that's the fake moving, not the upgrade.
#   2. Pin the candidate, init -upgrade from a filesystem mirror of this
#      tree's build, and run `show -json` on the old state, which is what
#      tooling reads. Then plan to a file and judge its JSON.
#   3. Destroy with the candidate, or with the previous release if the
#      candidate couldn't plan.
#
# A finding is a resource change, an output change, drift the previous
# release didn't see, a `show -json` that fails, or a plan that fails. A
# finding passes only if ci/upgrade-accepted.json lists it for this PREVIOUS,
# with a reason. An entry that can't match fails too, so the file can't go
# stale: one for this PREVIOUS that nothing matched, and in a run from the
# default PREVIOUS, one for any other release.
#
# Every resource type the previous release registers has to be in some
# directory's state, or be listed under not_covered with a reason. Otherwise
# a type's upgrade is never tried.
#
# Usage: ci/upgrade-against-fake.sh, or make upgrade-test.
#   PREVIOUS=0.1.0-rc.2   the release to upgrade from. Default: see
#                         default_previous
#   CLIS="terraform tofu" the CLIs to run, each must be on PATH
#   DIRS="look cluster"   a subset of the directories. Coverage is only
#                         checked when every directory runs
#   UPGRADE_KEEP=1        keep the run directory, to read the logs
#   UPGRADE_INIT_TIMEOUT=90  seconds for each of an init's three tries
#
# Requires: go, python3, ssh-keygen, curl, git with the release tags, and network
# to registry.terraform.io, registry.opentofu.org and the GitHub release
# assets they point at.

set -euo pipefail

cd "$(dirname "$0")/.."
readonly REPO="$PWD"
# shellcheck source=../scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"
# shellcheck source=../scripts/lib/walkthrough.sh
. "$REPO/scripts/lib/walkthrough.sh"

CLIS="${CLIS-terraform tofu}"
readonly CANDIDATE=0.0.0-upgrade-candidate
readonly ACCEPTED="$REPO/ci/upgrade-accepted.json"
# Each init downloads, so each try gets this long, and an outage fails the
# run in minutes rather than at the job's timeout.
readonly INIT_TIMEOUT="${UPGRADE_INIT_TIMEOUT:-90}"

# Set by use_cli, the CLI every tf call runs.
CLI=""
fake_pid=""

cleanup() {
  if [[ -n "$fake_pid" ]]; then
    kill "$fake_pid" 2>/dev/null || true
    # Reaped here, so bash doesn't print a "Terminated" line after the result.
    wait "$fake_pid" 2>/dev/null || true
  fi
  if [[ "${UPGRADE_KEEP:-0}" == 1 ]]; then
    note "run directory kept: $WORK"
  else
    rm -rf "$WORK"
  fi
}

fail() {
  printf '\nupgrade: FAIL - %s\n' "$1" >&2
  exit 1
}

# ci_warning <message> warns, and in GitHub Actions annotates the run too, so
# a skipped release shows on the pull request and not only in the log.
ci_warning() {
  warn "$1"
  if [[ "${GITHUB_ACTIONS:-}" == true ]]; then
    echo "::warning title=Upgrade check::$1"
  fi
}

# fetch_served writes the versions each registry serves, for default_previous.
fetch_served() {
  local host
  for host in registry.terraform.io registry.opentofu.org; do
    curl -fsS --connect-timeout 10 --max-time 30 --retry 3 --retry-all-errors \
      "https://$host/v1/providers/$NAMESPACE/$PROVIDER_NAME/versions" > "$WORK/versions-$host.json" \
      || fail "could not list the versions $host serves"
  done
}

# release_line sets RELEASE_LINE to X.Y when this run is for the
# release-vX.Y.x branch, and to nothing when it's for main. In GitHub Actions
# that's the branch a pull request merges into, or the branch pushed. Locally
# it's the release branch whose own commits, the ones main doesn't have, HEAD
# contains, so a backport branch made from release-v0.1.x is on 0.1 whatever
# its name.
release_line() {
  local branch="" line base count lines=""
  RELEASE_LINE=""
  if [[ -n "${GITHUB_BASE_REF:-}" ]]; then
    branch="$GITHUB_BASE_REF"
  elif [[ "${GITHUB_ACTIONS:-}" == true ]]; then
    branch="${GITHUB_REF_NAME:-}"
  else
    git -C "$REPO" rev-parse -q --verify origin/main >/dev/null \
      || fail "no origin/main to tell main from a release branch. git fetch origin, or set PREVIOUS"
    git -C "$REPO" for-each-ref --format='%(refname:short)' 'refs/remotes/origin/release-v*.x' > "$WORK/release-branches" \
      || fail "could not list the release branches"
    while read -r branch; do
      line="${branch#origin/release-v}"
      line="${line%.x}"
      [[ "$line" =~ ^[0-9]+\.[0-9]+$ ]] || continue
      base="$(git -C "$REPO" merge-base HEAD "$branch")" || continue
      count="$(git -C "$REPO" rev-list --count "origin/main..$base")" || fail "could not compare $branch with origin/main"
      [[ "$count" == 0 ]] || lines="$lines $line"
    done < "$WORK/release-branches"
    read -r -a lines <<< "$lines"
    [[ "${#lines[@]}" -le 1 ]] || fail "HEAD has commits from more than one release branch (${lines[*]}). Set PREVIOUS"
    RELEASE_LINE="${lines[0]-}"
    return 0
  fi
  if [[ "$branch" =~ ^release-v([0-9]+\.[0-9]+)\.x$ ]]; then
    RELEASE_LINE="${BASH_REMATCH[1]}"
  fi
  return 0
}

# default_previous sets DEFAULT_PREVIOUS to the release to upgrade from. For
# a release-vX.Y.x branch it's the newest release up to X.Y, so the newest
# X.Y release, or before the first one the newest of an earlier line. For
# main it's the newest release overall. Either way it leaves out a tag on
# HEAD, which is the version being built, and a tag either registry doesn't
# serve yet, which is a release still being published: it's skipped with a
# warning rather than failing every run until it lands.
default_previous() {
  local line
  release_line
  git -C "$REPO" tag -l 'v*' > "$WORK/tags-all" || fail "could not list the release tags"
  git -C "$REPO" tag --points-at HEAD -l 'v*' > "$WORK/tags-head" || fail "could not list the tags on HEAD"
  if python3 - "$WORK/tags-all" "$WORK/tags-head" "$WORK/versions-registry.terraform.io.json" \
    "$WORK/versions-registry.opentofu.org.json" "$RELEASE_LINE" "$WORK/skipped" > "$WORK/default-previous" <<'PY'
import json, re, sys

def key(tag):
    m = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?", tag)
    if not m:
        return None
    pre = m.group(4)
    # A release sorts after its pre-releases, and rc.10 after rc.2.
    parts = tuple((0, int(p), "") if p.isdigit() else (1, 0, p) for p in pre.split(".")) if pre else ()
    return (int(m.group(1)), int(m.group(2)), int(m.group(3)), 0 if pre else 1, parts)

every, here = (set(open(p).read().split()) for p in sys.argv[1:3])
served = {host: {v["version"] for v in json.load(open(p))["versions"]}
          for host, p in zip(("registry.terraform.io", "registry.opentofu.org"), sys.argv[3:5])}
line, skipped = sys.argv[5], open(sys.argv[6], "w")
limit = tuple(int(n) for n in line.split(".")) if line else None
tags = [t for t in every - here if key(t) and (not limit or key(t)[:2] <= limit)]
for t in sorted(tags, key=key, reverse=True):
    lacking = [h for h, vs in served.items() if t[1:] not in vs]
    if not lacking:
        print(t[1:])
        sys.exit(0)
    skipped.write("%s is tagged but %s doesn't serve it yet, so it's skipped\n" % (t, " and ".join(lacking)))
sys.exit(1)
PY
  then
    DEFAULT_PREVIOUS="$(cat "$WORK/default-previous")"
  else
    fail "no published release tag${RELEASE_LINE:+ up to $RELEASE_LINE} to upgrade from. git fetch --tags, or set PREVIOUS"
  fi
  while read -r line; do
    ci_warning "$line"
  done < "$WORK/skipped"
}

# check_accepted refuses an entry without a reason, so the file explains
# itself.
check_accepted() {
  python3 - "$ACCEPTED" <<'PY' || fail "ci/upgrade-accepted.json is malformed"
import json, sys
f = json.load(open(sys.argv[1]))
need = {"changes": ("from", "directory", "change", "reason"), "not_covered": ("type", "reason")}
bad = ["unknown key %s" % k for k in f if k not in ("about", *need)]
for section, fields in need.items():
    for e in f.get(section, []):
        if set(e) != set(fields) or not all(isinstance(e[k], str) and e[k].strip() for k in fields):
            bad.append("%s entry %s needs exactly %s, each non-empty" % (section, json.dumps(e), ", ".join(fields)))
for b in bad:
    print("   " + b)
sys.exit(1 if bad else 0)
PY
}

# use_cli <cli>: the CLI the next runs use, each with its own plugin cache so
# the previous release downloads once per CLI.
use_cli() {
  CLI="$1"
  need "$CLI"
  mkdir -p "$WORK/$CLI" "$WORK/cache-$CLI"
  export TF_PLUGIN_CACHE_DIR="$WORK/cache-$CLI"
}

# tf <dir> <rc file> <args...>: run $CLI in <dir> with that CLI configuration,
# output in <dir>/last.log, shown only on failure.
tf() {
  local dir="$1" rc="$2"
  shift 2
  if ! ( cd "$dir" && TF_CLI_CONFIG_FILE="$rc" "$CLI" "$@" ) > "$dir/last.log" 2>&1; then
    cat "$dir/last.log" >&2
    return 1
  fi
}

# tf_init <dir> <rc file> [args...]: init, three tries of INIT_TIMEOUT seconds
# each. The CLI is exec'd, so the timeout stops it and not just a subshell.
tf_init() {
  local dir="$1" rc="$2" try pid watcher status
  shift 2
  for try in 1 2 3; do
    ( cd "$dir" && TF_CLI_CONFIG_FILE="$rc" exec "$CLI" init -input=false -no-color "$@" ) > "$dir/last.log" 2>&1 &
    pid=$!
    ( sleep "$INIT_TIMEOUT"; kill -TERM "$pid"; sleep 10; kill -KILL "$pid" ) > /dev/null 2>&1 &
    watcher=$!
    status=0
    wait "$pid" || status=$?
    kill "$watcher" 2>/dev/null || true
    wait "$watcher" 2>/dev/null || true
    [[ "$status" == 0 ]] && return 0
    warn "init in $dir failed or took over ${INIT_TIMEOUT}s, try $try of 3"
    [[ "$try" == 3 ]] || sleep $((try * 5))
  done
  cat "$dir/last.log" >&2
  return 1
}

# pin <dir> <version>: the provider's version line in main.tf.
pin() {
  python3 - "$1/main.tf" "$2" "$NAMESPACE/$PROVIDER_NAME" <<'PY'
import re, sys
path, version, source = sys.argv[1:4]
text = open(path).read()
text = re.sub(r'\n *version *= *"[^"]*" # upgrade-check pin', "", text)
new, n = re.subn(r'(\n( *)source *= *"%s"\n)' % re.escape(source),
                 lambda m: '%s%sversion = "%s" # upgrade-check pin\n' % (m.group(1), m.group(2), version),
                 text, count=1)
if n != 1:
    sys.exit('no source = "%s" in %s' % (source, path))
open(path, "w").write(new)
PY
}

# init_previous <dir> [args...]: init the previous release from the registry,
# reusing the first directory's lock so the plugin cache serves it.
init_previous() {
  local dir="$1"
  shift
  pin "$dir" "$PREVIOUS"
  if [[ -f "$WORK/$CLI.lock.hcl" ]]; then
    cp "$WORK/$CLI.lock.hcl" "$dir/.terraform.lock.hcl"
  fi
  tf_init "$dir" "$WORK/registry.tfrc" "$@" || fail "$CLI: init of $PREVIOUS from the registry failed"
  if [[ ! -f "$WORK/$CLI.lock.hcl" ]]; then
    cp "$dir/.terraform.lock.hcl" "$WORK/$CLI.lock.hcl"
  fi
}

start_fake() {
  step "Start the fake from this tree"
  go build -o "$WORK/fakeapi" ./internal/tools/fakeapi || fail "the fake did not build"
  start_loopback_server "$WORK/fakeapi" -secrets-file "$WORK/fake-secrets.env" "$WORK/fake.url" "$WORK/fake.log" \
    || { cat "$WORK/fake.log" >&2; fail "the fake did not start"; }
  fake_pid="$SERVER_PID"
  # Its TF_CLI_CONFIG_FILE is the candidate's, which build_provider_mirror
  # wrote. Every tf call names the configuration it runs with anyway.
  export_fake_credential "$WORK" "$WORK/fake-secrets.env"
  export "${ENV_PREFIX}ENDPOINT=$SERVER_URL"
  unset "${ENV_PREFIX}REGION" "${ENV_PREFIX}API_KEY"
  # Both releases skip the 1s floor on a loopback endpoint. The default 10s
  # makes each wait on the fake take 10s.
  export "${ENV_PREFIX}POLL_INTERVAL=200ms"
  walkthrough_secrets "$WORK"
  good "the fake is up"
}

# pick_values applies look/ once with the previous release, before any
# directory runs, so every directory gets its values whichever DIRS runs. It
# also records the resource types the previous release registers.
pick_values() {
  local dir="$WORK/pick"
  step "Pick values from look, with $PREVIOUS"
  cp -R "$PREV_EXAMPLES/look" "$dir"
  init_previous "$dir"
  tf "$dir" "$WORK/registry.tfrc" apply -auto-approve -input=false -no-color || fail "look with $PREVIOUS failed"
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" "$CLI" output -json ) > "$dir/look.json" \
    || fail "look's output with $PREVIOUS failed"
  walkthrough_pick "$dir/look.json"

  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" "$CLI" providers schema -json ) > "$dir/schema.json" \
    || fail "could not read the schema of $PREVIOUS"
  python3 - "$dir/schema.json" "/$NAMESPACE/$PROVIDER_NAME" > "$WORK/registered" <<'PY'
import json, sys
schemas = json.load(open(sys.argv[1]))["provider_schemas"]
ours = [v for k, v in schemas.items() if k.endswith(sys.argv[2])]
for t in sorted(ours[0].get("resource_schemas", {})):
    print(t)
PY
  [[ -s "$WORK/registered" ]] || fail "$PREVIOUS registers no resource types, or its schema couldn't be read"
  good "$cluster_type in $region, organization $organization, project $project"
  good "$PREVIOUS registers $(wc -l < "$WORK/registered" | tr -d ' ') resource types"
}

# covered <dir>: the provider's resource types in <dir>'s state, one per line.
covered() {
  python3 - "$1/terraform.tfstate" "/$NAMESPACE/$PROVIDER_NAME\"]" <<'PY'
import json, sys
state = json.load(open(sys.argv[1]))
for r in state.get("resources", []):
    if r["mode"] == "managed" and r["provider"].endswith(sys.argv[2]) and r.get("instances"):
        print(r["type"])
PY
}

# judge <dir> <name>: findings from the candidate's plan and its show -json
# of the old state, against the accepted list. Writes <dir>/matched. Exits 0
# for nothing unaccepted, 3 for a finding, and anything else is a crash.
judge() {
  python3 - "$1" "$ACCEPTED" "$PREVIOUS" "$2" <<'PY'
import json, os, sys
d, accepted_path, previous, name = sys.argv[1:]
ok = {(a["directory"], a["change"]) for a in json.load(open(accepted_path)).get("changes", [])
      if a["from"] == previous}

def changed(c):
    b, a = c.get("before") or {}, c.get("after") or {}
    return sorted(k for k in set(b) | set(a) if b.get(k) != a.get(k))

def drift(path):
    p = json.load(open(path))
    return {x["address"]: changed(x["change"]) for x in p.get("resource_drift", [])}

def text(path):
    return " ".join(open(path).read().split())[:300] if os.path.getsize(path) else ""

found = []
if text(d + "/show.err"):
    found.append(("show -json fails", text(d + "/show.err")))
if text(d + "/plan.err"):
    found.append(("plan fails", text(d + "/plan.err")))
else:
    p = json.load(open(d + "/plan.json"))
    for rc in p.get("resource_changes", []):
        acts = rc["change"]["actions"]
        if acts == ["no-op"]:
            continue
        detail = "attributes: " + ", ".join(changed(rc["change"]))
        replace = [".".join(map(str, path)) for path in rc["change"].get("replace_paths", [])]
        if replace:
            detail += ", forces replacement: " + ", ".join(replace)
        found.append(("%s %s" % (rc["address"], "+".join(acts)), detail))
    for out, c in sorted(p.get("output_changes", {}).items()):
        if c.get("actions") != ["no-op"]:
            found.append(("output.%s %s" % (out, "+".join(c["actions"])), ""))
    # Drift is the candidate reading the old state back differently, such
    # as a renamed attribute coming back under its new name. The plan can
    # still be empty, but tooling that reads state sees the change. What the
    # previous release saw drift on too is the fake, not the upgrade.
    before = drift(d + "/baseline.json")
    for addr, attrs in sorted(drift(d + "/plan.json").items()):
        seen = set(before.get(addr, []))
        if seen:
            print("   the previous release saw this drift too, not counted: %s: %s" % (addr, ", ".join(sorted(seen))))
        new = [a for a in attrs if a not in seen]
        if new:
            found.append(("%s drift" % addr, "attributes: " + ", ".join(new)))

bad, matched = [], []
for change, detail in found:
    tag = "accepted" if (name, change) in ok else "NOT ACCEPTED"
    print("   %s: %s%s" % (tag, change, (" (" + detail + ")") if detail else ""))
    (matched if (name, change) in ok else bad).append(change)
open(d + "/matched", "w").write("".join("%s\t%s\n" % (name, m) for m in matched))
sys.exit(3 if bad else 0)
PY
}

# run_dir <name>: one directory, previous release then candidate.
run_dir() {
  local name="$1" src="$1" dir status=0 planned=1 plan_status
  [[ "$name" == full-everything ]] && src=full
  dir="$WORK/$CLI/$name"

  step "$CLI: $name"
  cp -R "$PREV_EXAMPLES/$src" "$dir" || fail "could not copy $src from v$PREVIOUS"
  walkthrough_tfvars "$name" "$PREV_EXAMPLES" "up-$CLI" > "$dir/terraform.tfvars.new" || fail "$name: no terraform.tfvars"
  if [[ -s "$dir/terraform.tfvars.new" ]]; then
    mv "$dir/terraform.tfvars.new" "$dir/terraform.tfvars"
  fi
  # 0.1.0-rc.1 and rc.2's full/ with own_project read the project quota
  # before the project exists (fixed in #58), and the fake now answers that
  # read 404 as the API does. Quota reads are data sources, so skipping them
  # loses no state to upgrade.
  if [[ "$name" == full-everything && "$PREVIOUS" =~ ^0\.1\.0-rc\.[12]$ ]]; then
    echo 'quota_checks = false' >> "$dir/terraform.tfvars"
  fi

  init_previous "$dir"
  tf "$dir" "$WORK/registry.tfrc" apply -auto-approve -input=false -no-color || fail "$CLI $name: apply with $PREVIOUS failed"
  # -detailed-exitcode is 0 for drift with no change, so the drift is read
  # from the plan's JSON.
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" "$CLI" plan -detailed-exitcode -input=false -no-color -out=baseline.plan ) > "$dir/baseline.log" 2>&1 || status=$?
  if [[ "$status" != 0 ]]; then
    cat "$dir/baseline.log" >&2
    fail "$CLI $name: $PREVIOUS does not plan empty on its own state (exit $status), so an upgrade diff can't be told apart"
  fi
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" "$CLI" show -json baseline.plan ) > "$dir/baseline.json" \
    || fail "$CLI $name: show -json of $PREVIOUS's own plan failed"
  covered "$dir" >> "$WORK/covered"
  good "$PREVIOUS: applied, then planned empty"

  pin "$dir" "$CANDIDATE"
  tf_init "$dir" "$WORK/terraform.rc" -upgrade || fail "$CLI $name: init of the candidate failed"
  grep -q "$CANDIDATE" "$dir/last.log" || { cat "$dir/last.log" >&2; fail "$CLI $name: init did not install the candidate"; }

  # Before the plan, so the state is still the one the previous release wrote.
  status=0
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/terraform.rc" "$CLI" show -json -no-color ) > /dev/null 2> "$dir/show.err" || status=$?
  if [[ "$status" == 0 ]]; then
    : > "$dir/show.err"
  elif [[ ! -s "$dir/show.err" ]]; then
    echo "exit $status" > "$dir/show.err"
  fi

  status=0
  : > "$dir/plan.err"
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/terraform.rc" "$CLI" plan -detailed-exitcode -input=false -no-color -out=upgrade.plan ) > "$dir/upgrade.log" 2>&1 || status=$?
  plan_status="$status"
  if [[ "$status" == 0 || "$status" == 2 ]]; then
    ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/terraform.rc" "$CLI" show -json upgrade.plan ) > "$dir/plan.json" \
      || fail "$CLI $name: show -json of the candidate's plan failed"
  else
    planned=0
    grep -m1 -A1 '^Error: ' "$dir/upgrade.log" > "$dir/plan.err" || echo "exit $status" > "$dir/plan.err"
  fi

  : > "$dir/matched"
  status=0
  judge "$dir" "$name" || status=$?
  case "$status" in
    0) good "candidate on $PREVIOUS state: nothing unaccepted"
       RESULTS+=("$CLI $name: ok (plan exit $plan_status)") ;;
    3) RESULTS+=("$CLI $name: FAIL (plan exit $plan_status)")
       FAILED=1 ;;
    *) RESULTS+=("$CLI $name: FAIL, the judge exited $status")
       FAILED=1 ;;
  esac
  cat "$dir/matched" >> "$WORK/matched-all"

  if [[ "$planned" == 1 ]]; then
    tf "$dir" "$WORK/terraform.rc" destroy -auto-approve -input=false -no-color || fail "$CLI $name: the candidate's destroy failed"
    good "destroyed with the candidate"
  else
    # The candidate can't plan this state, so the release that made it
    # destroys it, and the fake is clean for the next directory.
    init_previous "$dir" -upgrade
    tf "$dir" "$WORK/registry.tfrc" destroy -auto-approve -input=false -no-color || fail "$CLI $name: the destroy with $PREVIOUS failed"
    good "destroyed with $PREVIOUS, since the candidate couldn't plan"
  fi
}

# check_stale fails on an accepted change that can't match: one for this
# PREVIOUS, in a directory that ran, that nothing matched, and in a run from
# the default PREVIOUS, one for any other release.
check_stale() {
  python3 - "$ACCEPTED" "$PREVIOUS" "$DEFAULT_PREVIOUS" "$WORK/matched-all" "${DIRS[*]}" <<'PY'
import json, sys
acc, prev, default, matched, dirs = json.load(open(sys.argv[1])), sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5].split()
seen = {tuple(l.split("\t", 1)) for l in open(matched).read().splitlines() if "\t" in l}
stale = 0
for a in acc.get("changes", []):
    if a["from"] == prev and a["directory"] in dirs and (a["directory"], a["change"]) not in seen:
        print("   stale accepted change: %s %s, from %s, matched nothing" % (a["directory"], a["change"], prev))
        stale = 1
    elif a["from"] != prev and prev == default:
        print("   stale accepted change: %s %s is from %s, but the check upgrades from %s" % (a["directory"], a["change"], a["from"], default))
        stale = 1
sys.exit(stale)
PY
}

# check_coverage fails on a type the previous release registers that no
# directory's state held, unless not_covered lists it, and on a not_covered
# entry for a type that was covered.
check_coverage() {
  python3 - "$ACCEPTED" "$WORK/registered" "$WORK/covered" <<'PY'
import json, sys
acc = {e["type"]: e["reason"] for e in json.load(open(sys.argv[1])).get("not_covered", [])}
registered = set(open(sys.argv[2]).read().split())
covered = set(open(sys.argv[3]).read().split())
bad = 0
for t in sorted(registered - covered):
    if t in acc:
        print("   not covered, accepted: %s (%s)" % (t, acc[t]))
    else:
        print("   NOT COVERED: %s is in no walkthrough directory's state" % t)
        bad = 1
for t in sorted(set(acc) & covered):
    print("   stale not_covered entry: %s is covered" % t)
    bad = 1
print("   %d of %d registered resource types covered" % (len(registered & covered), len(registered)))
sys.exit(bad)
PY
}

main() {
  local d cli first_cli
  read_names
  need go
  need python3
  need ssh-keygen
  need curl
  WORK="$(private_tmpdir)" || exit 1
  readonly WORK
  trap cleanup EXIT

  check_accepted
  fetch_served
  default_previous
  PREVIOUS="${PREVIOUS:-$DEFAULT_PREVIOUS}"
  readonly PREVIOUS DEFAULT_PREVIOUS
  git -C "$REPO" rev-parse -q --verify "refs/tags/v$PREVIOUS" >/dev/null || fail "no tag v$PREVIOUS. git fetch --tags"
  if [[ "$PREVIOUS" == "$DEFAULT_PREVIOUS" ]]; then
    note "upgrading from $PREVIOUS"
  else
    warn "upgrading from $PREVIOUS, not the default $DEFAULT_PREVIOUS. Accepted changes for other releases aren't read"
  fi

  mkdir -p "$WORK/previous"
  git -C "$REPO" archive -o "$WORK/previous.tar" "v$PREVIOUS" examples/walkthrough \
    || fail "could not read examples/walkthrough from v$PREVIOUS"
  tar -x -f "$WORK/previous.tar" -C "$WORK/previous" || fail "could not unpack examples/walkthrough from v$PREVIOUS"
  PREV_EXAMPLES="$WORK/previous/examples/walkthrough"

  # The directories come from the previous release, so a new one is run
  # without a change here.
  read -r -a ALL_DIRS <<< "$(walkthrough_dirs "$PREV_EXAMPLES" | tr '\n' ' ')"
  [[ "${#ALL_DIRS[@]}" -gt 0 ]] || fail "v$PREVIOUS's examples/walkthrough has no directory the fake can apply"
  read -r -a DIRS <<< "${DIRS-${ALL_DIRS[*]}}"
  [[ "${#DIRS[@]}" -gt 0 ]] || fail "DIRS is empty. Leave it unset for every directory: ${ALL_DIRS[*]}"
  for d in "${DIRS[@]}"; do
    [[ " ${ALL_DIRS[*]} " == *" $d "* ]] \
      || fail "DIRS names $d, which isn't a directory in v$PREVIOUS's examples/walkthrough that the fake can apply. Those are: ${ALL_DIRS[*]}"
  done
  : > "$WORK/matched-all"
  : > "$WORK/covered"

  step "Build the candidate from this tree"
  build_provider_mirror "$WORK" "$CANDIDATE"
  printf 'provider_installation {\n  direct {}\n}\n' > "$WORK/registry.tfrc"
  start_fake

  read -r first_cli _ <<< "$CLIS"
  [[ -n "$first_cli" ]] || fail "CLIS is empty. Leave it unset for terraform and tofu"
  use_cli "$first_cli"
  pick_values
  # Every directory's terraform.tfvars, before anything is applied, so a
  # variable nothing fills in fails the run at once.
  for d in "${DIRS[@]}"; do
    walkthrough_tfvars "$d" "$PREV_EXAMPLES" up > /dev/null || fail "$d: no terraform.tfvars"
  done

  RESULTS=()
  FAILED=0
  for cli in $CLIS; do
    use_cli "$cli"
    for d in "${DIRS[@]}"; do
      run_dir "$d"
    done
  done

  step "Accepted changes and coverage"
  if ! check_stale; then
    FAILED=1
    RESULTS+=("ci/upgrade-accepted.json: stale changes")
  fi
  if [[ "$(printf '%s\n' "${DIRS[@]}" | sort -u)" == "$(printf '%s\n' "${ALL_DIRS[@]}" | sort -u)" ]]; then
    if ! check_coverage; then
      FAILED=1
      RESULTS+=("coverage: a registered resource type is in no directory's state")
    fi
  else
    warn "coverage not checked: DIRS runs only some directories"
  fi

  step "Results, $PREVIOUS to this tree"
  printf '   %s\n' "${RESULTS[@]}"
  note "wall clock: ${SECONDS}s"
  [[ "$FAILED" == 0 ]] || fail "see the results above: a finding from $PREVIOUS that ci/upgrade-accepted.json doesn't list, a stale entry there, a resource type no directory covers, or a judge that failed"
  printf '\n%sState from %s plans empty under this tree, but for accepted changes.%s\n' "$GREEN" "$PREVIOUS" "$R"
}

main "$@"
