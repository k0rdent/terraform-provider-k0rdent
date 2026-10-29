#!/usr/bin/env bash
#
# State written by the previous published release, installed from the public
# registry, must plan empty under the provider built from this tree, with the
# previous release's configuration. Runs over examples/walkthrough against
# the fake, under terraform and tofu.
#
# For each CLI and each directory the fake can apply:
#   1. Copy examples/walkthrough/<dir> from tag v$PREVIOUS, pin
#      version = "$PREVIOUS", init from the registry, apply, and plan again:
#      the previous release must plan empty on its own state.
#   2. Pin the candidate, init -upgrade from a filesystem mirror of this
#      tree's build, and run `show -json` on the old state, which is what
#      tooling reads. Then plan to a file and judge its JSON.
#   3. Destroy with the candidate.
#
# A finding is any resource change, output change or drift (an attribute the
# candidate reads back differently) that isn't a no-op, and a `show -json`
# that fails. A finding passes only if ci/upgrade-accepted.json lists it for
# this PREVIOUS, with a reason. An accepted entry that matches nothing fails
# too, so the file can't go stale.
#
# Every resource type the previous release registers has to be in some
# directory's state, or be listed under not_covered with a reason. Otherwise
# a type's upgrade is never tried.
#
# Usage: ci/upgrade-against-fake.sh, or make upgrade-test.
#   PREVIOUS=0.1.0-rc.2   the release to upgrade from. Default: the newest
#                         published release on HEAD's line, see
#                         previous_release
#   CLIS="terraform tofu" the CLIs to run, each must be on PATH
#   DIRS="look cluster"   a subset of the directories. Coverage is only
#                         checked when every directory runs
#   UPGRADE_KEEP=1        keep the run directory, to read the logs
#
# Requires: go, python3, ssh-keygen, curl, git with the release tags, and network
# to registry.terraform.io, registry.opentofu.org and the GitHub release
# assets they point at.

set -euo pipefail

cd "$(dirname "$0")/.."
readonly REPO="$PWD"
# shellcheck source=../scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"

readonly ALL_DIRS="look cluster cluster-own-project network full full-everything"
CLIS="${CLIS:-terraform tofu}"
DIRS="${DIRS:-$ALL_DIRS}"
readonly CANDIDATE=0.0.0-upgrade-candidate
readonly ACCEPTED="$REPO/ci/upgrade-accepted.json"

# Set by pick_values from what look/ reports, read by tfvars.
region=""
cluster_type=""
filtered_type=""
organization=""
project=""

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

# fetch_served writes the versions each registry serves, for previous_release.
fetch_served() {
  local host
  for host in registry.terraform.io registry.opentofu.org; do
    curl -fsS --retry 3 "https://$host/v1/providers/$NAMESPACE/$PROVIDER_NAME/versions" > "$WORK/versions-$host.json" \
      || fail "could not list the versions $host serves"
  done
}

# previous_release prints the release to upgrade from: the newest tag on the
# line (major.minor) of the newest release tag HEAD contains, that both
# registries serve. On main that's the newest release cut from main's line,
# though it sits on a release branch. On a release branch it's that branch's
# newest. A tag on HEAD itself is the version being built, so it's left out.
# A tag the registries don't serve yet is a release still being published,
# and is skipped rather than failing every run until it lands.
previous_release() {
  python3 - <(git -C "$REPO" tag -l 'v*') <(git -C "$REPO" tag --merged HEAD -l 'v*') \
    <(git -C "$REPO" tag --points-at HEAD -l 'v*') \
    "$WORK/versions-registry.terraform.io.json" "$WORK/versions-registry.opentofu.org.json" <<'PY'
import json, re, sys

def key(tag):
    m = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?", tag)
    if not m:
        return None
    pre = m.group(4)
    # A release sorts after its pre-releases, and rc.10 after rc.2.
    parts = tuple((0, int(p), "") if p.isdigit() else (1, 0, p) for p in pre.split(".")) if pre else ()
    return (int(m.group(1)), int(m.group(2)), int(m.group(3)), 0 if pre else 1, parts)

every, reachable, here = (set(open(p).read().split()) for p in sys.argv[1:4])
served = set.intersection(*({v["version"] for v in json.load(open(p))["versions"]} for p in sys.argv[4:6]))
every = {t for t in every - here if key(t)}
reachable = [t for t in reachable & every]
if not reachable:
    sys.exit(1)
line = key(max(reachable, key=key))[:2]
for t in sorted((t for t in every if key(t)[:2] == line), key=key, reverse=True):
    if t[1:] in served:
        print(t[1:])
        sys.exit(0)
    print("   %s is tagged but not on both registries yet, so it's skipped" % t, file=sys.stderr)
sys.exit(1)
PY
}

# check_accepted refuses an entry without a reason, so the file explains
# itself.
check_accepted() {
  python3 - "$ACCEPTED" <<'PY' || fail "ci/upgrade-accepted.json is malformed"
import json, sys
f = json.load(open(sys.argv[1]))
need = {"changes": ("from", "directory", "change", "reason"), "not_covered": ("type", "reason")}
bad = [k for k in f if k not in ("about", *need)]
for section, fields in need.items():
    for e in f.get(section, []):
        if set(e) != set(fields) or not all(isinstance(e[k], str) and e[k].strip() for k in fields):
            bad.append("%s entry %s needs exactly %s, each non-empty" % (section, json.dumps(e), ", ".join(fields)))
for b in bad:
    print("   " + b)
sys.exit(1 if bad else 0)
PY
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

# pin <dir> <version>: the k0rdent provider's version line in main.tf.
pin() {
  python3 - "$1/main.tf" "$2" <<'PY'
import re, sys
path, version = sys.argv[1], sys.argv[2]
text = open(path).read()
text = re.sub(r'\n *version *= *"[^"]*" # upgrade-check pin', "", text)
new, n = re.subn(r'(\n( *)source *= *"k0rdent/k0rdent"\n)',
                 lambda m: '%s%sversion = "%s" # upgrade-check pin\n' % (m.group(1), m.group(2), version),
                 text, count=1)
if n != 1:
    sys.exit("no source = \"k0rdent/k0rdent\" in " + path)
open(path, "w").write(new)
PY
}

# The candidate in a filesystem mirror for both registry hosts, so terraform
# and tofu each find it under the address the configuration names.
build_candidate() {
  local platform host
  step "Build the candidate from this tree"
  platform="$(go env GOOS)_$(go env GOARCH)"
  go build -ldflags "-X main.version=$CANDIDATE" -o "$WORK/candidate-bin" . || fail "the build failed"
  for host in registry.terraform.io registry.opentofu.org; do
    mkdir -p "$WORK/mirror/$host/$NAMESPACE/$PROVIDER_NAME/$CANDIDATE/$platform"
    cp "$WORK/candidate-bin" "$WORK/mirror/$host/$NAMESPACE/$PROVIDER_NAME/$CANDIDATE/$platform/terraform-provider-${PROVIDER_NAME}_v$CANDIDATE"
  done
  printf 'provider_installation {\n  direct {}\n}\n' > "$WORK/registry.tfrc"
  cat > "$WORK/candidate.tfrc" <<EOF
provider_installation {
  filesystem_mirror {
    path    = "$WORK/mirror"
    include = ["registry.terraform.io/$NAMESPACE/$PROVIDER_NAME", "registry.opentofu.org/$NAMESPACE/$PROVIDER_NAME"]
  }
  direct {
    exclude = ["registry.terraform.io/$NAMESPACE/$PROVIDER_NAME", "registry.opentofu.org/$NAMESPACE/$PROVIDER_NAME"]
  }
}
EOF
  good "built $CANDIDATE into the mirror"
}

start_fake() {
  step "Start the fake from this tree"
  go build -o "$WORK/fakeapi" ./internal/tools/fakeapi
  start_loopback_server "$WORK/fakeapi" -secrets-file "$WORK/fake-secrets.env" "$WORK/fake.url" "$WORK/fake.log" \
    || { cat "$WORK/fake.log"; fail "the fake did not start"; }
  fake_pid="$SERVER_PID"
  export_fake_credential "$WORK" "$WORK/fake-secrets.env"
  unset TF_CLI_CONFIG_FILE
  export "${ENV_PREFIX}ENDPOINT=$SERVER_URL"
  unset "${ENV_PREFIX}REGION" "${ENV_PREFIX}API_KEY"
  # Both releases skip the 1s floor on a loopback endpoint. The default 10s
  # makes each wait on the fake take 10s.
  export "${ENV_PREFIX}POLL_INTERVAL=200ms"
  ssh-keygen -q -t ed25519 -N '' -C upgrade@ci -f "$WORK/id_ed25519"
  TF_VAR_webhook_secret="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
  export TF_VAR_webhook_secret
  good "the fake is up"
}

# use_cli <cli>: the CLI the next runs use, each with its own plugin cache so
# the previous release downloads once per CLI.
use_cli() {
  CLI="$1"
  need "$CLI"
  mkdir -p "$WORK/$CLI" "$WORK/cache-$CLI"
  export TF_PLUGIN_CACHE_DIR="$WORK/cache-$CLI"
}

# init_previous <dir>: init the previous release from the registry, reusing
# the first directory's lock so the plugin cache serves it.
init_previous() {
  local dir="$1"
  pin "$dir" "$PREVIOUS"
  [[ -f "$WORK/$CLI.lock.hcl" ]] && cp "$WORK/$CLI.lock.hcl" "$dir/.terraform.lock.hcl"
  tf "$dir" "$WORK/registry.tfrc" init -input=false -no-color || fail "$CLI: init of $PREVIOUS from the registry failed"
  [[ -f "$WORK/$CLI.lock.hcl" ]] || cp "$dir/.terraform.lock.hcl" "$WORK/$CLI.lock.hcl"
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
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" "$CLI" output -json ) > "$dir/look.json"
  python3 - "$dir/look.json" > "$WORK/picked" <<'PY'
import json, sys
out = json.load(open(sys.argv[1]))
region = next(r for r, types in sorted(out["cluster_types"]["value"].items()) if types)
projects = out["projects"]["value"]
project = next((p for p in projects if (", region %s " % region) in p), projects[0])
types = out["cluster_types"]["value"][region]
# full/ puts a security group on its cluster, and the fake's cpu-general nodes
# have no interface to put it on.
filtered = next((t for t in types if t != "cpu-general"), types[0])
print(region, types[0], filtered,
      out["organizations"]["value"][0].split(" ")[0], project.split(" ")[0])
PY
  read -r region cluster_type filtered_type organization project < "$WORK/picked"

  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" "$CLI" providers schema -json ) > "$dir/schema.json"
  python3 - "$dir/schema.json" "/$NAMESPACE/$PROVIDER_NAME" > "$WORK/registered" <<'PY'
import json, sys
schemas = json.load(open(sys.argv[1]))["provider_schemas"]
ours = [v for k, v in schemas.items() if k.endswith(sys.argv[2])]
for t in sorted(ours[0].get("resource_schemas", {})):
    print(t)
PY
  [[ -s "$WORK/registered" ]] || fail "$PREVIOUS registers no resource types, or its schema couldn't be read"
  good "$cluster_type in $region, organization $organization, project $project; $(wc -l < "$WORK/registered" | tr -d ' ') resource types registered"
}

# tfvars <prefix> <name>: the terraform.tfvars for that run, as
# ci/walkthrough-against-fake.sh fills them.
tfvars() {
  local prefix="$1"
  case "$2" in
    look) ;;
    cluster) cat <<EOF
prefix              = "$prefix"
region              = "$region"
project             = "$project"
cluster_type        = "$cluster_type"
ssh_public_key_file = "$WORK/id_ed25519.pub"
EOF
      ;;
    cluster-own-project) cat <<EOF
prefix       = "$prefix"
region       = "$region"
organization = "$organization"
cluster_type = "$cluster_type"
EOF
      ;;
    network) cat <<EOF
prefix       = "$prefix"
region       = "$region"
project      = "$project"
allowed_cidr = "203.0.113.0/24"
EOF
      ;;
    full | full-everything) cat <<EOF
prefix              = "$prefix"
region              = "$region"
project             = "$project"
organization        = "$organization"
cluster_type        = "$filtered_type"
ssh_public_key_file = "$WORK/id_ed25519.pub"
allowed_cidrs       = ["203.0.113.0/24", "198.51.100.0/25"]
webhook_url         = "https://hooks.example.com/k0rdent"
EOF
      if [[ "$2" == full ]]; then
        sed -n '/^build = {/,/^}/p' "$PREV_EXAMPLES/full/terraform.tfvars"
      else
        cat <<'EOF'
build = {
  network             = true
  security_group      = true
  vpc_security_groups = true
  webhook             = true
  instance_group      = true
  file_store          = true
  vpc_peering         = true
  own_project         = true
}
EOF
      fi
      ;;
    *) fail "no terraform.tfvars for $2" ;;
  esac
}

# covered <dir>: the k0rdent resource types in <dir>'s state, one per line.
covered() {
  python3 - "$1/terraform.tfstate" "/$NAMESPACE/$PROVIDER_NAME\"]" <<'PY'
import json, sys
state = json.load(open(sys.argv[1]))
for r in state.get("resources", []):
    if r["mode"] == "managed" and r["provider"].endswith(sys.argv[2]) and r.get("instances"):
        print(r["type"])
PY
}

# judge <dir> <name>: findings from the candidate's plan JSON and its show
# -json of the state, against the accepted list. Writes <dir>/matched.
judge() {
  python3 - "$1/plan.json" "$1/show.err" "$ACCEPTED" "$PREVIOUS" "$2" "$1/matched" <<'PY'
import json, os, sys
plan_path, show_err, accepted_path, previous, name, matched_path = sys.argv[1:]
p = json.load(open(plan_path))
ok = {(a["directory"], a["change"]) for a in json.load(open(accepted_path)).get("changes", [])
      if a["from"] == previous}

def changed(c):
    b, a = c.get("before") or {}, c.get("after") or {}
    return sorted(k for k in set(b) | set(a) if b.get(k) != a.get(k))

found = []
if os.path.getsize(show_err):
    found.append(("show -json fails", " ".join(open(show_err).read().split())[:300]))
for rc in p.get("resource_changes", []):
    acts = rc["change"]["actions"]
    if acts == ["no-op"]:
        continue
    detail = "attributes: " + ", ".join(changed(rc["change"]))
    replace = [".".join(map(str, path)) for path in rc["change"].get("replace_paths", [])]
    if replace:
        detail += "; forces replacement: " + ", ".join(replace)
    found.append(("%s %s" % (rc["address"], "+".join(acts)), detail))
for out, c in sorted(p.get("output_changes", {}).items()):
    if c.get("actions") != ["no-op"]:
        found.append(("output.%s %s" % (out, "+".join(c["actions"])), ""))
# Drift is the candidate reading the old state back differently, such as a
# renamed attribute coming back under its new name. The plan can still be
# empty, but tooling that reads state sees the change.
for d in p.get("resource_drift", []):
    found.append(("%s drift" % d["address"], "attributes: " + ", ".join(changed(d["change"]))))

bad, matched = [], []
for change, detail in found:
    tag = "accepted" if (name, change) in ok else "NOT ACCEPTED"
    print("   %s: %s%s" % (tag, change, (" (" + detail + ")") if detail else ""))
    (matched if (name, change) in ok else bad).append(change)
open(matched_path, "w").write("".join("%s\t%s\n" % (name, m) for m in matched))
sys.exit(1 if bad else 0)
PY
}

# run_dir <name>: one directory, previous release then candidate.
run_dir() {
  local name="$1" src="$1" dir status=0
  [[ "$name" == full-everything ]] && src=full
  dir="$WORK/$CLI/$name"

  step "$CLI: $name"
  cp -R "$PREV_EXAMPLES/$src" "$dir"
  tfvars "up-$CLI" "$name" > "$dir/terraform.tfvars.new"
  [[ -s "$dir/terraform.tfvars.new" ]] && mv "$dir/terraform.tfvars.new" "$dir/terraform.tfvars"

  init_previous "$dir"
  tf "$dir" "$WORK/registry.tfrc" apply -auto-approve -input=false -no-color || fail "$CLI $name: apply with $PREVIOUS failed"
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/registry.tfrc" "$CLI" plan -detailed-exitcode -input=false -no-color ) > "$dir/baseline.log" 2>&1 || status=$?
  if [[ "$status" != 0 ]]; then
    cat "$dir/baseline.log" >&2
    fail "$CLI $name: $PREVIOUS does not plan empty on its own state (exit $status), so an upgrade diff can't be told apart"
  fi
  covered "$dir" >> "$WORK/covered"
  good "$PREVIOUS: applied, then planned empty"

  pin "$dir" "$CANDIDATE"
  tf "$dir" "$WORK/candidate.tfrc" init -upgrade -input=false -no-color || fail "$CLI $name: init of the candidate failed"
  grep -q "$CANDIDATE" "$dir/last.log" || { cat "$dir/last.log"; fail "$CLI $name: init did not install the candidate"; }

  # Before the plan, so the state is still the one the previous release wrote.
  status=0
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/candidate.tfrc" "$CLI" show -json -no-color ) > "$dir/show.json" 2> "$dir/show.err" || status=$?
  if [[ "$status" == 0 ]]; then
    : > "$dir/show.err"
  elif [[ ! -s "$dir/show.err" ]]; then
    echo "exit $status" > "$dir/show.err"
  fi

  status=0
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/candidate.tfrc" "$CLI" plan -detailed-exitcode -input=false -no-color -out=upgrade.plan ) > "$dir/upgrade.log" 2>&1 || status=$?
  [[ "$status" == 0 || "$status" == 2 ]] || { cat "$dir/upgrade.log" >&2; fail "$CLI $name: the candidate's plan failed (exit $status)"; }
  ( cd "$dir" && TF_CLI_CONFIG_FILE="$WORK/candidate.tfrc" "$CLI" show -json upgrade.plan ) > "$dir/plan.json" \
    || fail "$CLI $name: show -json of the candidate's plan failed"
  if judge "$dir" "$name"; then
    good "candidate on $PREVIOUS state: nothing unaccepted (plan exit $status)"
    RESULTS+=("$CLI $name: ok (plan exit $status)")
  else
    awk '/will perform the following actions|^Changes to Outputs|^Note: Objects have changed/ {on=1} on' "$dir/upgrade.log" >&2
    RESULTS+=("$CLI $name: FAIL")
    FAILED=1
  fi
  cat "$dir/matched" >> "$WORK/matched-all"

  tf "$dir" "$WORK/candidate.tfrc" destroy -auto-approve -input=false -no-color || fail "$CLI $name: the candidate's destroy failed"
  good "destroyed with the candidate"
}

# check_stale fails on an accepted change for this PREVIOUS, in a directory
# that ran, that nothing matched.
check_stale() {
  python3 - "$ACCEPTED" "$PREVIOUS" "$WORK/matched-all" "$DIRS" <<'PY'
import json, sys
acc, prev, matched, dirs = json.load(open(sys.argv[1])), sys.argv[2], sys.argv[3], sys.argv[4].split()
seen = {tuple(l.split("\t", 1)) for l in open(matched).read().splitlines() if "\t" in l}
stale = [a for a in acc.get("changes", [])
         if a["from"] == prev and a["directory"] in dirs and (a["directory"], a["change"]) not in seen]
for a in stale:
    print("   stale accepted change: %s %s" % (a["directory"], a["change"]))
sys.exit(1 if stale else 0)
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
  local d first_cli
  read_names
  need go
  need python3
  need ssh-keygen
  need curl
  WORK="$(private_tmpdir)" || exit 1
  readonly WORK
  trap cleanup EXIT

  for d in $DIRS; do
    [[ " $ALL_DIRS " == *" $d "* ]] || fail "no directory $d. DIRS takes: $ALL_DIRS"
  done
  check_accepted
  if [[ -z "${PREVIOUS:-}" ]]; then
    fetch_served
    PREVIOUS="$(previous_release)" || fail "no published release tag on HEAD's line to upgrade from. git fetch --tags, or set PREVIOUS"
  fi
  readonly PREVIOUS
  git -C "$REPO" rev-parse -q --verify "refs/tags/v$PREVIOUS" >/dev/null || fail "no tag v$PREVIOUS. git fetch --tags"
  note "upgrading from $PREVIOUS"
  mkdir -p "$WORK/previous"
  git -C "$REPO" archive "v$PREVIOUS" examples/walkthrough | tar -x -C "$WORK/previous"
  PREV_EXAMPLES="$WORK/previous/examples/walkthrough"
  : > "$WORK/matched-all"
  : > "$WORK/covered"

  build_candidate
  start_fake

  read -r first_cli _ <<< "$CLIS"
  use_cli "$first_cli"
  pick_values

  RESULTS=()
  FAILED=0
  for CLI in $CLIS; do
    use_cli "$CLI"
    for d in $DIRS; do
      run_dir "$d"
    done
  done

  step "Accepted changes and coverage"
  if ! check_stale; then
    FAILED=1
    RESULTS+=("ci/upgrade-accepted.json: stale changes")
  fi
  if [[ "$(printf '%s\n' $DIRS | sort -u)" == "$(printf '%s\n' $ALL_DIRS | sort -u)" ]]; then
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
  [[ "$FAILED" == 0 ]] || fail "a finding from $PREVIOUS isn't in ci/upgrade-accepted.json, an entry there is stale, or a resource type isn't covered"
  printf '\n%sState from %s plans empty under this tree, but for accepted changes.%s\n' "$GREEN" "$PREVIOUS" "$R"
}

main "$@"
