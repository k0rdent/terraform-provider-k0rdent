#!/usr/bin/env bash
#
# Runs the walkthrough in examples/walkthrough against the fake, the way
# docs/walkthrough-hardware.md tells a reader to: source setup.sh, apply look,
# then copy a directory, fill in its terraform.tfvars and apply, plan again,
# destroy. Every directory runs, network/ again through walkthrough.sh, and
# full/ twice, as shipped and with every switch on, so a change that breaks
# any of them fails CI here rather than on real hardware.
#
# Each run fills in terraform.tfvars, the one file a reader edits, so the
# directories run exactly as shipped otherwise.
#
# Usage: ci/walkthrough-against-fake.sh, or make walkthrough-test.
#
# Requires: go, python3, ssh-keygen, and `terraform` or `tofu` on PATH.
#
# Exit status: 0 when every directory works, 1 at the first step that fails.

set -euo pipefail

cd "$(dirname "$0")/.."
readonly REPO="$PWD"
# shellcheck source=../scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"

readonly EXAMPLES="$REPO/examples/walkthrough"
readonly DOC="$REPO/docs/walkthrough-hardware.md"

# Set by start_fake, read by cleanup.
fake_pid=""

# Set by run_quick_path to the run folder walkthrough.sh setup made.
quick_run=""

# Set by run_look from what look/ reports, read by the directories after it.
region=""
cluster_type=""
filtered_type=""
organization=""
project=""

# Helpers

cleanup() {
  if [[ -n "$fake_pid" ]]; then
    kill "$fake_pid" 2>/dev/null || true
  fi
  # walkthrough.sh setup made worktrees under WORK. Remove only those, and
  # leave every other worktree of the clone as it is.
  local real wt
  real=$(cd "$WORK" 2>/dev/null && pwd -P) || real=$WORK
  git -C "$REPO" worktree list --porcelain | sed -n 's/^worktree //p' | while read -r wt; do
    case "$wt" in
      "$WORK"/* | "$real"/*) git -C "$REPO" worktree remove --force "$wt" ;;
    esac
  done
  rm -rf "$WORK"
}

fail() {
  printf '\nwalkthrough: FAIL - %s\n' "$1" >&2
  exit 1
}

# tf <name> <args...> runs Terraform in the copy of <name> under WORK, with its
# output in last.log. The log only reaches the terminal when the command fails.
tf() {
  local dir="$WORK/$1"
  shift

  if ! ( cd "$dir" && "$TF_BIN" "$@" -no-color ) > "$dir/last.log" 2>&1; then
    cat "$dir/last.log" >&2
    return 1
  fi
}

# refuses_until_filled <name>: an empty terraform.tfvars stops the plan,
# naming the value to set.
refuses_until_filled() {
  local dir="$WORK/$1" status=0

  ( cd "$dir" && "$TF_BIN" plan -input=false -no-color ) > "$dir/unset.log" 2>&1 || status=$?
  if [[ "$status" == 0 ]] || ! grep -q 'Set prefix in terraform.tfvars' "$dir/unset.log"; then
    cat "$dir/unset.log" >&2
    fail "$1: an empty terraform.tfvars did not stop the plan by name"
  fi
  good "the plan refuses until terraform.tfvars is filled in"
}

# plans_clean <name> <when>: a plan right after an apply proposes nothing.
plans_clean() {
  local dir="$WORK/$1" status=0

  ( cd "$dir" && "$TF_BIN" plan -detailed-exitcode -input=false -no-color ) > "$dir/replan.log" 2>&1 || status=$?
  if [[ "$status" != 0 ]]; then
    cat "$dir/replan.log" >&2
    fail "$1: the plan $2 exited $status, want 0"
  fi
}

# output_is <name> <output> <a Python expression on v, the output's value>
output_is() {
  local dir="$WORK/$1"

  ( cd "$dir" && "$TF_BIN" output -json "$2" ) > "$dir/output.json" || fail "$1: no output $2"
  if ! python3 - "$dir/output.json" "$3" <<'PY'
import json, sys
v = json.load(open(sys.argv[1]))
sys.exit(0 if eval(sys.argv[2], {}, {"v": v}) else 1)
PY
  then
    cat "$dir/output.json" >&2
    fail "$1: output $2 fails $3"
  fi
}

# Checks before anything runs

check_page() {
  local d

  step "The page and the directories agree"
  for d in look cluster cluster-own-project nodes network full; do
    if [[ ! -f "$EXAMPLES/$d/main.tf" ]]; then
      fail "examples/walkthrough/$d/main.tf is missing"
    fi
    if ! grep -q "examples/walkthrough/$d" "$DOC"; then
      fail "the page never names examples/walkthrough/$d"
    fi
  done
  for d in setup.sh walkthrough.sh; do
    if ! grep -q "examples/walkthrough/$d" "$DOC"; then
      fail "the page never names examples/walkthrough/$d"
    fi
  done

  # terraform.tfvars ships empty, so nobody applies someone else's ids, and the
  # plan refuses until it's filled in.
  for d in cluster cluster-own-project nodes network full; do
    if grep -Eq '^(prefix|region|project|organization|cluster_type|allowed_cidrs?|webhook_url) += +\[? *"[^"]' "$EXAMPLES/$d/terraform.tfvars"; then
      fail "examples/walkthrough/$d/terraform.tfvars ships a value"
    fi
  done
  good "every directory is on the page"
}

# The provider build and the fake

start_fake() {
  build_provider "$WORK" 0.0.0-walkthrough
  go build -o "$WORK/fakeapi" ./internal/tools/fakeapi

  if ! start_loopback_server "$WORK/fakeapi" -secrets-file "$WORK/fake-secrets.env" "$WORK/fake.url" "$WORK/fake.log"; then
    cat "$WORK/fake.log"
    fail "the fake did not start"
  fi
  fake_pid="$SERVER_PID"

  # shellcheck source=/dev/null # written by internal/tools/fakeapi
  . "$WORK/fake-secrets.env"
  export TF_IN_AUTOMATION=1
}

# setup.sh, sourced as a reader does

run_setup() {
  step "Source setup.sh"

  # Where docs/build-from-source.md puts the build's override.
  export XDG_CACHE_HOME="$WORK/cache"
  unset TF_CLI_CONFIG_FILE
  # shellcheck source=../examples/walkthrough/setup.sh
  if ( . "$EXAMPLES/setup.sh" </dev/null ) 2>/dev/null; then
    fail "setup.sh succeeded with no provider build"
  fi

  mkdir -p "$XDG_CACHE_HOME/k0rdent-dev"
  cp "$WORK/terraform.rc" "$XDG_CACHE_HOME/k0rdent-dev/terraform.rc"
  export "${ENV_PREFIX}REGION=left-over" "${ENV_PREFIX}API_KEY=left-over"
  printf '%s\n%s\n%s\n' "$SERVER_URL" "$FAKE_CLIENT_ID" "$FAKE_CLIENT_SECRET" > "$WORK/answers"

  # shellcheck source=../examples/walkthrough/setup.sh
  if ! . "$EXAMPLES/setup.sh" < "$WORK/answers"; then
    fail "setup.sh failed"
  fi
  if [[ "${TF_CLI_CONFIG_FILE:-}" != "$XDG_CACHE_HOME/k0rdent-dev/terraform.rc" ]]; then
    fail "setup.sh did not point Terraform at the build"
  fi
  if [[ "${K0RDENT_ENDPOINT:-}" != "$SERVER_URL" ]]; then
    fail "setup.sh did not set the endpoint"
  fi
  if [[ -n "${K0RDENT_REGION:-}${K0RDENT_API_KEY:-}" ]]; then
    fail "setup.sh left another landscape's settings"
  fi
  good "the environment is set"
}

# look/

run_look() {
  local dir="$WORK/look"

  step "look"
  cp -R "$EXAMPLES/look" "$dir"
  tf look apply -auto-approve -input=false || fail "the look apply failed"
  if ! grep -q '0 added, 0 changed, 0 destroyed' "$dir/last.log"; then
    fail "look changed something"
  fi

  tf look output -json
  python3 - "$dir/last.log" > "$WORK/picked" <<'PY'
import json, sys
out = json.load(open(sys.argv[1]))
region = next(r for r, types in sorted(out["cluster_types"]["value"].items()) if types)
projects = out["projects"]["value"]
project = next((p for p in projects if (", region %s " % region) in p), projects[0])
types = out["cluster_types"]["value"][region]
# full/ puts a security group on its cluster. The fake's cpu-general is the
# one type whose nodes have no interface to put it on, as on a landscape.
filtered = next((t for t in types if t != "cpu-general"), types[0])
print(region, types[0], filtered,
      out["organizations"]["value"][0].split(" ")[0], project.split(" ")[0])
PY
  read -r region cluster_type filtered_type organization project < "$WORK/picked"
  good "picked $cluster_type in $region, organization $organization, project $project, and $filtered_type for full/"
}

# cluster/ and cluster-own-project/

# A runner has no SSH key of its own, so make a throwaway one for the nodes.
make_ssh_key() {
  ssh-keygen -q -t ed25519 -N '' -C walkthrough@ci -f "$WORK/id_ed25519"
}

# run_cluster <directory> <the variable naming where it builds> <its value> [more tfvars]
run_cluster() {
  local name="$1" where_var="$2" where="$3" more="${4:-}"
  local dir="$WORK/$name"

  step "$name"
  cp -R "$EXAMPLES/$name" "$dir"
  refuses_until_filled "$name"

  cat > "$dir/terraform.tfvars" <<EOF
prefix       = "ci"
region       = "$region"
$where_var = "$where"
cluster_type = "$cluster_type"
$more
EOF
  tf "$name" plan -input=false || fail "$name: the plan failed"
  tf "$name" apply -auto-approve -input=false || fail "$name: the apply failed"
  if [[ -n "$more" ]]; then
    tf "$name" show
    if ! grep -q 'username *= "ops"' "$dir/last.log"; then
      fail "$name: the cluster has no ssh_access in state"
    fi
    good "ssh_access is on the cluster"
  fi

  plans_clean "$name" "after the apply"
  good "second plan: exit 0, no changes"

  # The ${var.prefix} is Terraform's, so sed must see it as written.
  # shellcheck disable=SC2016
  sed -i.bak 's/^  display_name = "${var.prefix} walkthrough"$/  display_name = "${var.prefix} walkthrough, changed"/' "$dir/main.tf"
  if ! grep -q 'walkthrough, changed' "$dir/main.tf"; then
    fail "$name: main.tf has no cluster display_name to change"
  fi
  tf "$name" plan -input=false || fail "$name: the plan for the change failed"
  if ! grep -q '1 to change, 0 to destroy' "$dir/last.log"; then
    cat "$dir/last.log" >&2
    fail "$name: the change is not one update in place"
  fi
  tf "$name" apply -auto-approve -input=false || fail "$name: the change failed"
  plans_clean "$name" "after the change"
  good "changed in place, then no changes"

  tf "$name" destroy -auto-approve -input=false || fail "$name: the destroy failed"
  good "destroyed"
}

# nodes/

# nodes/ needs a real cluster's API server, which the fake doesn't run, so it
# is only initialised and validated here. That still catches a change to the
# kubernetes provider, or to the kubeconfig's attributes, that breaks it.
run_nodes() {
  step "nodes"
  cp -R "$EXAMPLES/nodes" "$WORK/nodes"
  tf nodes init -input=false -backend=false || fail "nodes: init failed"
  tf nodes validate || fail "nodes: validate failed"
  good "valid"
}

# network/

run_network() {
  local dir="$WORK/network"

  step network
  cp -R "$EXAMPLES/network" "$dir"
  refuses_until_filled network

  cat > "$dir/terraform.tfvars" <<EOF
prefix       = "ci"
region       = "$region"
project      = "$project"
allowed_cidr = "203.0.113.0/24"
EOF
  tf network plan -input=false || fail "network: the plan failed"
  if ! grep -q '3 to add, 0 to change, 0 to destroy' "$dir/last.log"; then
    cat "$dir/last.log" >&2
    fail "network: the plan is not one network, one subnet and one group"
  fi

  tf network apply -auto-approve -input=false || fail "network: the apply failed"
  tf network output
  cat "$dir/last.log"
  output_is network subnet '"/" in (v["cidr"] or "")'
  output_is network security_group 'len(v["rules"]) == 15 and any(" 22 from 203.0.113.0/24" in r for r in v["rules"]) and any(r.startswith("in:  deny tcp 22 from 0.0.0.0/0") for r in v["rules"]) and not any(r.startswith("out: permit any") for r in v["rules"])'
  good "a subnet with its CIDR, and a group with its rules"

  plans_clean network "after the apply"
  good "second plan: exit 0, no changes"

  tf network destroy -auto-approve -input=false || fail "network: the destroy failed"
  good "destroyed"
}

# walkthrough.sh, the quick path

# quick <walkthrough.sh args...> runs walkthrough.sh in the run folder
# run_quick_path's setup made, after sourcing that folder's setup.sh as a
# reader does. Its output is in quick.log.
quick() {
  # shellcheck source=../examples/walkthrough/setup.sh
  ( cd "$quick_run/walkthrough" && . ./setup.sh < "$WORK/answers" &&
    ./walkthrough.sh "$@" ) > "$WORK/quick.log" 2>&1
}

quick_fails() {
  cat "$WORK/quick.log" >&2
  fail "walkthrough.sh: $1"
}

# run_quick_path runs `walkthrough.sh setup` on this tree, then `up` and
# `down` on network/ in the run folder it made, as a reader in a hurry would.
# The tree may have changes not committed yet, so setup gets a commit of it
# from git stash create, which changes nothing in the tree or the stash.
run_quick_path() {
  local ref log="$WORK/quick.log"

  step "walkthrough.sh"
  ref="$(cd "$REPO" && GIT_AUTHOR_NAME=ci GIT_AUTHOR_EMAIL=ci@localhost \
    GIT_COMMITTER_NAME=ci GIT_COMMITTER_EMAIL=ci@localhost git stash create)"
  mkdir -p "$WORK/config/k0rdent-walkthrough"
  cat > "$WORK/config/k0rdent-walkthrough/network.tfvars" <<EOF
prefix       = "quick"
region       = "$region"
project      = "$project"
allowed_cidr = "203.0.113.0/24"
EOF
  XDG_CONFIG_HOME="$WORK/config" WALKTHROUGH_RUNS="$WORK/runs" \
    "$EXAMPLES/walkthrough.sh" setup "${ref:-HEAD}" > "$log" 2>&1 || quick_fails "setup failed"
  quick_run="$(echo "$WORK/runs/"*)"
  if [[ ! -x "$quick_run/bin/terraform-provider-$PROVIDER_NAME" || ! -f "$quick_run/terraform.rc" ]]; then
    quick_fails "setup left no build or no terraform.rc in $quick_run"
  fi
  grep -q '^\$ go build -o ' "$log" || quick_fails "setup did not print its commands"
  grep -q '"quick"' "$quick_run/walkthrough/network/terraform.tfvars" ||
    quick_fails "setup did not copy network.tfvars from the config directory"
  grep -q '^prefix *= *""' "$quick_run/walkthrough/cluster/terraform.tfvars" ||
    quick_fails "setup filled in a terraform.tfvars it had no file for"
  good "setup made a run folder: a worktree, its build, and the walkthrough"

  if ( cd "$quick_run/walkthrough" && ./walkthrough.sh up network ) > "$log" 2>&1 ||
    ! grep -q 'setup.sh in ' "$log"; then
    quick_fails "up ran with another build's terraform.rc in the shell"
  fi
  good "up refuses until setup.sh points the shell at this run's build"

  if quick up cluster || ! grep -q 'Fill in cluster/terraform.tfvars' "$log"; then
    quick_fails "up ran with an empty terraform.tfvars"
  fi
  grep -q "Provider build: $quick_run/bin" "$log" ||
    quick_fails "setup.sh did not name the run's build"
  good "setup.sh names the run's build, and up refuses an empty terraform.tfvars"

  if ! quick up network -auto-approve -input=false || ! grep -q 'No changes after the apply' "$log"; then
    quick_fails "up network failed"
  fi
  [[ -f "$quick_run/walkthrough/network/terraform.tfstate" ]] ||
    quick_fails "the state is not in the run folder"
  good "up: plan, apply, then a plan with no changes, with the state in the run"

  if ! quick down network -auto-approve -input=false || ! grep -q 'Destroy complete' "$log"; then
    quick_fails "down network failed"
  fi
  good "down destroyed it"
}

# full/

# full/'s webhook signs with a secret from the shell, never from a file.
make_webhook_secret() {
  TF_VAR_webhook_secret="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
  export TF_VAR_webhook_secret
}

# run_full <name> <the build block> <a Python expression on the scorecard, v>
run_full() {
  local name="$1" build="$2" scorecard="$3"
  local dir="$WORK/$name"

  step "$name"
  cp -R "$EXAMPLES/full" "$dir"
  refuses_until_filled "$name"

  cat > "$dir/terraform.tfvars" <<EOF
prefix              = "ci"
region              = "$region"
project             = "$project"
organization        = "$organization"
cluster_type        = "$filtered_type"
ssh_public_key_file = "$WORK/id_ed25519.pub"
allowed_cidrs       = ["203.0.113.0/24", "198.51.100.0/25"]
webhook_url         = "https://hooks.example.com/k0rdent"

$build
EOF
  tf "$name" plan -input=false || fail "$name: the plan failed"

  # A first plan that warns teaches people to skim past warnings. The
  # provider build's own warning is the only one allowed.
  if grep '^Warning: ' "$dir/last.log" | grep -qv 'Provider development overrides are in effect'; then
    cat "$dir/last.log" >&2
    fail "$name: the first plan warns"
  fi
  good "the first plan has no warnings"

  tf "$name" apply -auto-approve -input=false || fail "$name: the apply failed"
  tf "$name" output
  cat "$dir/last.log"
  output_is "$name" scorecard "$scorecard"

  # What the platform reported back, as the scorecard says it.
  output_is "$name" scorecard 'v["quota"] == "within every limit" and v["kubeconfig"].startswith("active,") and v["tags landed"] == "network yes, security group yes"'
  output_is "$name" scorecard 'v["security group on the cluster"].endswith("rules: 19 sent, 19 on the platform") and v["webhook"].endswith(", active, subscriptions to this URL: 1")'

  output_is "$name" cluster 'v["state"] == "active" and v["ssh_users"] == ["ops"]'
  output_is "$name" network 'sorted(v["subnets"]) == ["nodes", "storage"] and all("/28" in s for s in v["subnets"].values())'
  output_is "$name" filtering 'len(v["rules"]) == 19 and sum(" from 198.51.100.0/25" in r for r in v["rules"]) == 3 and sum(r.startswith("in:  deny ") and " from 0.0.0.0/0 " in r for r in v["rules"]) == 2 and not any(r.startswith("in:  deny tcp 6443") for r in v["rules"]) and not any(r.startswith("out: permit any") for r in v["rules"]) and any(r.startswith("out: permit tcp from port 22 ") for r in v["rules"])'
  good "the scorecard says what ran and what was off"

  # look again, now that it has something to read by id.
  tf look apply -auto-approve -input=false || fail "$name: look failed with $name built"
  if ! grep -q '0 added, 0 changed, 0 destroyed' "$WORK/look/last.log"; then
    fail "$name: look changed something"
  fi
  output_is look webhooks 'len(v) == 1 and all(s.startswith("active") for s in v.values())'
  if [[ "$name" == full ]]; then
    output_is look in_project 'v["id"] == "'"$project"' in '"$region"'" and any(n.startswith("ci-walkthrough ") for n in v["networks"]) and v["vpcs"] and v["subnets"]'
  fi
  good "look reads what $name built"

  plans_clean "$name" "after the apply"
  good "second plan: exit 0, no changes"

  tf "$name" destroy -auto-approve -input=false || fail "$name: the destroy failed"
  good "destroyed"
}

# As shipped: the switches terraform.tfvars ships with.
run_full_as_shipped() {
  run_full full "$(sed -n '/^build = {/,/^}/p' "$EXAMPLES/full/terraform.tfvars")" \
    'v["webhook"].startswith("made ") and v["instance group"].startswith("off.")'
}

run_full_everything() {
  run_full full-everything 'build = {
  network             = true
  security_group      = true
  vpc_security_groups = true
  webhook             = true
  instance_group      = true
  file_store          = true
  vpc_peering         = true
  own_project         = true
}' 'not any(s.startswith("off.") for s in v.values()) and v["instance group"].endswith("1 of 1 hosts ready") and v["file store"].endswith(", settled") and v["vpc peering"].endswith("the cluster'"'"'s side active")'
}

main() {
  read_names
  find_terraform

  WORK="$(private_tmpdir)" || exit 1
  readonly WORK
  trap cleanup EXIT

  check_page
  start_fake
  run_setup
  run_look
  make_ssh_key
  run_cluster cluster project "$project" "ssh_public_key_file = \"$WORK/id_ed25519.pub\""
  run_cluster cluster-own-project organization "$organization"
  run_nodes
  run_network
  run_quick_path
  make_webhook_secret
  run_full_as_shipped
  run_full_everything

  printf '\n%sexamples/walkthrough works against the fake, every directory.%s\n' "$GREEN" "$R"
}

main "$@"
