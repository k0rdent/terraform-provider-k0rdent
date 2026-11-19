#!/usr/bin/env bash
#
# Tests ci/upgrade-against-fake.sh in a throwaway repository with stand-ins
# for terraform, go, curl and the kind stack's check, so nothing here needs
# a registry, a kind stack, a landscape or a credential:
#
#   * the fake path runs end to end: every directory applies with the
#     previous release, plans empty on its own state, takes the candidate,
#     plans the accepted drift and destroys, and the coverage check counts
#     every registered resource type.
#   * UPGRADE_REAL=1 refuses, before it builds anything, a missing endpoint,
#     a missing region or project, an endpoint off this machine that
#     UPGRADE_REAL_CONFIRM doesn't name or doesn't match, an off-machine
#     endpoint over http, a landscape with no credential and one with no
#     cluster type, and a kind stack whose stack-check refuses.
#   * a kind run runs real-api-check.sh stack-check first, runs the
#     walkthroughs as the organization's account, drops full/'s subnet pool
#     check, turns off what a kind stack can't serve and polls at 200ms,
#     reads the kind not_covered entries, and doesn't see the fake's
#     instance group drift.
#   * a hardware run leaves cluster-own-project out, turns full/'s webhook
#     and own_project off, names the cluster type and address pool the
#     runner set, destroys every directory before the next one starts, and
#     reads the hardware not_covered entries.
#   * the accepted file's landscape rules: a not_covered entry this
#     landscape needs can't be missing, a landscapeless one can't name a
#     covered type, a change entry for this landscape that matches nothing
#     fails the run, and an unknown landscape is refused.
#
# The run against the real CLIs and the real registries is make upgrade-test,
# which CI's upgrade job runs, and a real run against a kind stack or a
# landscape is make upgrade-test-real. This test needs only bash, python3,
# git and ssh-keygen.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
kept_dirs=""
trap 'rm -rf "$work" $kept_dirs' EXIT

fail() { echo "upgrade-against-fake test: FAIL - $1" >&2; exit 1; }

# The throwaway repository: this tree's check and libraries, a stub for the
# kind stack's check, and a walkthrough as v0.1.0 shipped it.
repo="$work/repo"
mkdir -p "$repo/scripts/lib" "$repo/ci" "$repo/internal/tools/fakeapi" \
  "$repo/examples/walkthrough/look" "$repo/examples/walkthrough/cluster" \
  "$repo/examples/walkthrough/cluster-own-project" "$repo/examples/walkthrough/full" \
  "$repo/examples/walkthrough/network" "$work/tmp"
cp "$here/ci/upgrade-against-fake.sh" "$repo/ci/"
cp "$here/ci/upgrade-accepted.json" "$repo/ci/"
cp "$here/scripts/lib/common.sh" "$here/scripts/lib/walkthrough.sh" "$repo/scripts/lib/"
echo 'module example.com/terraform-provider-k0rdent' > "$repo/go.mod"
echo 'package main' > "$repo/main.go"
echo 'package main' > "$repo/internal/tools/fakeapi/main.go"

cat > "$repo/scripts/real-api-check.sh" <<'EOF'
#!/usr/bin/env bash
# A stand-in for the real check: stack-check records that it ran, and
# answers what STUB_STACK_CHECK says.
echo "stack-check $*" >> "${STUB_STACK_LOG:-/dev/null}"
if [ "${STUB_STACK_CHECK:-ok}" != ok ]; then
  echo "the stack holds what an earlier run left (stand-in)" >&2
  exit 1
fi
EOF
chmod +x "$repo/scripts/real-api-check.sh"

# The walkthrough directories, as v0.1.0 shipped them. full/main.tf carries
# the subnet pool postcondition the kind landscape drops, and every main.tf
# the source line the version pin looks for.
tf_block='terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

provider "k0rdent" {}
'
printf '%s' "$tf_block" > "$repo/examples/walkthrough/look/main.tf"
printf '%sresource "k0rdent_kubernetes_cluster" "walkthrough" {}\n' "$tf_block" \
  > "$repo/examples/walkthrough/cluster/main.tf"
printf '%sresource "k0rdent_project" "own" {}\nresource "k0rdent_kubernetes_cluster" "walkthrough" {}\n' "$tf_block" \
  > "$repo/examples/walkthrough/cluster-own-project/main.tf"
printf '%sresource "k0rdent_network" "this" {}\nresource "k0rdent_subnet" "this" {}\nresource "k0rdent_security_group" "this" {}\n' "$tf_block" \
  > "$repo/examples/walkthrough/network/main.tf"
printf '%sresource "k0rdent_kubernetes_cluster" "this" {}\nresource "k0rdent_network" "this" {}\nresource "k0rdent_subnet" "this" {}\nresource "k0rdent_security_group" "this" {}\n\ndata "k0rdent_address_pool" "subnet_pool" {\n  lifecycle {\n    postcondition {\n      condition = self.cidr != "" && true\n      error_message = "Subnet ${self.id} came back outside its pool."\n    }\n  }\n}\n' "$tf_block" \
  > "$repo/examples/walkthrough/full/main.tf"

cat > "$repo/examples/walkthrough/cluster/terraform.tfvars" <<'EOF'
prefix       = ""
region       = ""
project      = ""
cluster_type = ""

ssh_public_key_file = ""
EOF
cat > "$repo/examples/walkthrough/cluster-own-project/terraform.tfvars" <<'EOF'
prefix       = ""
region       = ""
organization = ""
cluster_type = ""
EOF
cat > "$repo/examples/walkthrough/network/terraform.tfvars" <<'EOF'
prefix  = ""
region  = ""
project = ""

allowed_cidr = ""
EOF
cat > "$repo/examples/walkthrough/full/terraform.tfvars" <<'EOF'
prefix       = ""
region       = ""
project      = ""
organization = ""
cluster_type = ""

ssh_public_key_file = ""
webhook_url         = ""
allowed_cidrs       = []

build = {
  network        = true
  security_group = true
  webhook        = true
  instance_group = false
  file_store     = false
  vpc_peering    = false
  own_project    = false
}
EOF

git -C "$repo" init -q
git -C "$repo" add -A
git -C "$repo" -c user.name=ci -c user.email=ci@example.com commit -q -m "the walkthroughs v0.1.0 shipped"
git -C "$repo" tag v0.1.0
echo "a candidate change, so the tag is not on HEAD" > "$repo/README.md"
git -C "$repo" add -A
git -C "$repo" -c user.name=ci -c user.email=ci@example.com commit -q -m candidate
git -C "$repo" update-ref refs/remotes/origin/main HEAD

# What look prints and the previous release's schema, which the terraform
# stand-in serves. look lists one region with two cluster types, one project
# in it and one organization.
cat > "$work/look.json" <<'EOF'
{"cluster_types": {"value": {"r1": ["ct-a", "ct-b"]}},
 "projects": {"value": ["p1, region r1 x"]},
 "organizations": {"value": ["org1 (x)"]}}
EOF
cat > "$work/schema.json" <<'EOF'
{"provider_schemas": {"registry.terraform.io/k0rdent/k0rdent": {"resource_schemas": {
  "k0rdent_file_store": {}, "k0rdent_instance_group": {}, "k0rdent_kubernetes_cluster": {},
  "k0rdent_network": {}, "k0rdent_project": {}, "k0rdent_security_group": {},
  "k0rdent_subnet": {}, "k0rdent_vpc_peering": {}, "k0rdent_webhook": {}}}}}
EOF

# The stand-ins, on PATH ahead of the real tools.
mkdir -p "$work/bin"
cat > "$work/bin/go" <<'EOF'
#!/usr/bin/env bash
# A stand-in for go: the name audit's answer, the platform for the mirror's
# path, and builds that write a stand-in binary. A build is logged, so the
# test can tell the check refused before it built anything.
set -u
case "$1" in
  run)
    printf 'Provider type name             : k0rdent\nRegistry namespace             : k0rdent\n'
    ;;
  env)
    case "$2" in GOOS) echo darwin ;; GOARCH) echo arm64 ;; *) exit 1 ;; esac
    ;;
  build)
    echo "build $*" >> "${STUB_GO_LOG:-/dev/null}"
    args="$*"
    out=""
    while [ $# -gt 0 ]; do
      [ "$1" = "-o" ] && out="$2"
      shift
    done
    [ -n "$out" ] || exit 1
    case "$args" in
      *fakeapi*)
        cat > "$out" <<'SH'
#!/bin/sh
# A stand-in for the fake: it writes the secrets file the starter waits
# for, prints a URL and stays up until it is killed.
secrets=""
while [ $# -gt 0 ]; do
  [ "$1" = "-secrets-file" ] && secrets="$2"
  shift
done
printf 'FAKE_CLIENT_ID=fake-client\nFAKE_CLIENT_SECRET=fake-secret\n' > "$secrets"
echo "http://127.0.0.1:$((19000 + $$ % 900))"
exec sleep 300
SH
        ;;
      *) printf '#!/bin/sh\nexit 0\n' > "$out" ;;
    esac
    chmod +x "$out"
    ;;
  *) echo "go $* must not run" >&2; exit 1 ;;
esac
EOF
cat > "$work/bin/curl" <<'EOF'
#!/usr/bin/env bash
# A stand-in for curl: both registries' version listings, with the one
# release the throwaway repository tagged. The check reads nothing else.
for a in "$@"; do
  case "$a" in
    https://registry.terraform.io/v1/providers/k0rdent/k0rdent/versions | \
    https://registry.opentofu.org/v1/providers/k0rdent/k0rdent/versions)
      printf '{"versions":[{"version":"0.1.0"}]}\n'
      exit 0
      ;;
  esac
done
echo "curl $* must not run" >&2
exit 1
EOF
cat > "$work/bin/terraform" <<'EOF'
#!/usr/bin/env bash
# A stand-in for terraform. TF_CLI_CONFIG_FILE tells the release: the
# previous one runs with the registry's configuration, the candidate with
# the mirror's. State is a function of the directory and of the build
# switches its terraform.tfvars ends with, so what a landscape turned off
# never lands in state. A plan's drift is a function of the release and the
# landscape, matching what the accepted file lists for 0.1.0. Every command
# is logged, with the credential and poll interval it ran with.
set -u
rel=unknown
case "${TF_CLI_CONFIG_FILE:-}" in
  *registry.tfrc) rel=previous ;;
  *terraform.rc) rel=candidate ;;
esac
[ "$rel" != unknown ] || { echo "tf: no known CLI configuration" >&2; exit 1; }
cmd="$1"
shift
printf '%s %s %s client=%s poll=%s\n' "$rel" "$cmd" "${PWD##*/}" \
  "${K0RDENT_CLIENT_ID:-none}" "${K0RDENT_POLL_INTERVAL:-default}" >> "${STUB_TF_LOG:-/dev/null}"
case "$cmd" in
  init)
    [ -f .terraform.lock.hcl ] || printf '# stand-in lock\n' > .terraform.lock.hcl
    if [ "$rel" = candidate ]; then
      echo "Installed k0rdent/k0rdent v0.0.0-upgrade-candidate"
    else
      echo "Installed k0rdent/k0rdent v0.1.0"
    fi
    ;;
  apply)
    STUB_REL="$rel" python3 - <<'PY'
import json, os, re
d = os.path.basename(os.getcwd())
types = {"pick": [], "look": [],
         "cluster": ["k0rdent_kubernetes_cluster"],
         "cluster-own-project": ["k0rdent_project", "k0rdent_kubernetes_cluster"],
         "network": ["k0rdent_network", "k0rdent_subnet", "k0rdent_security_group"]}.get(d)
if types is None:  # full and full-everything, from the build switches
    types = ["k0rdent_kubernetes_cluster", "k0rdent_network", "k0rdent_subnet", "k0rdent_security_group"]
    on, in_build = set(), False
    for line in open("terraform.tfvars").read().splitlines():
        if line.startswith("build = {"):
            in_build = True
        elif in_build and line.startswith("}"):
            in_build = False
        elif in_build:
            m = re.match(r"^  ([a-z_]+)\s*=\s*true\b", line)
            if m:
                on.add(m.group(1))
    for s, t in (("webhook", "k0rdent_webhook"), ("instance_group", "k0rdent_instance_group"),
                 ("file_store", "k0rdent_file_store"), ("vpc_peering", "k0rdent_vpc_peering"),
                 ("own_project", "k0rdent_project")):
        if s in on:
            types.append(t)
state = {"version": 4, "resources": [
    {"mode": "managed", "type": t, "provider": 'provider["registry.terraform.io/k0rdent/k0rdent"]',
     "instances": [{"attributes": {"id": "stub"}}]} for t in sorted(set(types))]}
json.dump(state, open("terraform.tfstate", "w"), indent=2)
PY
    ;;
  output)
    cat "${STUB_LOOK_JSON:?}"
    ;;
  providers)
    cat "${STUB_SCHEMA_JSON:?}"
    ;;
  plan)
    out=""
    for a in "$@"; do case "$a" in -out=*) out="${a#-out=}" ;; esac; done
    [ -n "$out" ] || { echo "tf: plan without -out" >&2; exit 1; }
    STUB_REL="$rel" STUB_LANDSCAPE="${STUB_LANDSCAPE:-fake}" python3 - "$out.shim" <<'PY'
import json, os, sys
d = os.path.basename(os.getcwd())
drift = []
if os.environ["STUB_REL"] == "candidate":
    addr = {"cluster": "k0rdent_kubernetes_cluster.walkthrough",
            "cluster-own-project": "k0rdent_kubernetes_cluster.walkthrough",
            "full": "k0rdent_kubernetes_cluster.this",
            "full-everything": "k0rdent_kubernetes_cluster.this"}.get(d)
    if addr:  # the candidate reads the cluster's new uid and endpoint back
        drift.append({"address": addr, "change": {"before": {}, "after": {"uid": "u", "api_endpoint": "e"}}})
    if d == "full-everything" and os.environ["STUB_LANDSCAPE"] == "fake":
        drift.append({"address": "k0rdent_instance_group.this[0]",
                      "change": {"before": {}, "after": {"uid": "u"}}})
json.dump({"resource_drift": drift, "resource_changes": [], "output_changes": {}}, open(sys.argv[1], "w"))
PY
    ;;
  show)
    file=""
    for a in "$@"; do case "$a" in -*) ;; *) file="$a" ;; esac; done
    if [ -n "$file" ]; then cat "$file.shim"; else cat terraform.tfstate 2>/dev/null || echo '{}'; fi
    ;;
  destroy)
    rm -f terraform.tfstate
    ;;
  *) echo "tf: $cmd must not run" >&2; exit 1 ;;
esac
EOF
chmod +x "$work/bin/"*

# upgrade [VAR=value...] runs the check in a clean environment with the
# stand-ins on PATH. The run directory is kept, for the assertions below.
upgrade() {
  env -i PATH="$work/bin:$PATH" HOME="$work" TMPDIR="$work/tmp" \
    CLIS=terraform UPGRADE_PACE=0 UPGRADE_KEEP=1 \
    STUB_LOOK_JSON="$work/look.json" STUB_SCHEMA_JSON="$work/schema.json" \
    STUB_TF_LOG="$work/tf.log" STUB_GO_LOG="$work/go.log" STUB_STACK_LOG="$work/stack.log" \
    "$@" "$repo/ci/upgrade-against-fake.sh"
}

# fake_run, kind_run and hw_run name the environment each landscape runs in.
fake_run() { upgrade "$@"; }
kind_run() {
  upgrade UPGRADE_REAL=1 K0RDENT_ENDPOINT=http://localhost:14000 K0RDENT_REGION=r1 K0RDENT_PROJECT=p1 \
    REAL_API_ORG_CLIENT_ID=org-client REAL_API_ORG_CLIENT_SECRET=org-secret STUB_LANDSCAPE=kind "$@"
}
hw_run() {
  upgrade UPGRADE_REAL=1 UPGRADE_REAL_CONFIRM=api.example.com \
    K0RDENT_ENDPOINT=https://api.example.com K0RDENT_REGION=hw-r K0RDENT_PROJECT=hw-p \
    K0RDENT_CLIENT_ID=hw-client K0RDENT_CLIENT_SECRET=hw-secret \
    REAL_API_CLUSTER_TYPE=ct-hw REAL_API_ADDRESS_POOL=pool-7 STUB_LANDSCAPE=hardware "$@"
}

passes() {
  local what="$1"; shift
  "$@" > "$work/out" 2>&1 || { cat "$work/out" >&2; fail "$what: exited non-zero"; }
}
fails_with() {
  local what="$1" text="$2"; shift 2
  if "$@" > "$work/out" 2>&1; then cat "$work/out" >&2; fail "$what: passed"; fi
  grep -q "$text" "$work/out" || { cat "$work/out" >&2; fail "$what: no \"$text\" in the output"; }
}
want() { grep -q "$2" "$work/out" || { cat "$work/out" >&2; fail "$1: no \"$2\" in the output"; }; }
no_want() { ! grep -q "$2" "$work/out" || { cat "$work/out" >&2; fail "$1: \"$2\" in the output"; }; }

# keep_run reads the run directory the check kept, and remembers it for the
# cleanup trap, so a failure on a real macOS doesn't leave it behind.
keep_run() {
  local dir
  dir="$(sed -n 's/^.*run directory kept: //p' "$work/out" | tail -1)"
  [ -n "$dir" ] || { cat "$work/out" >&2; fail "$1: the check kept no run directory"; }
  kept_dirs="$kept_dirs $dir"
  printf '%s' "$dir"
}

# --- the fake path runs end to end -------------------------------------------

: > "$work/tf.log"; : > "$work/go.log"; : > "$work/stack.log"
passes "the fake run" fake_run
want "the fake run" "State from 0.1.0 plans empty under this tree, but for accepted changes."
want "the fake run" "landscape: fake"
want "the fake run" "9 of 9 registered resource types covered"
want "the fake run" "accepted: k0rdent_instance_group.this\[0\] drift"
grep -q "client=fake-client" "$work/tf.log" || { cat "$work/tf.log" >&2; fail "the fake run didn't use the fake's credential"; }
grep -q "client=none" "$work/tf.log" && { cat "$work/tf.log" >&2; fail "the fake run ran a Terraform command with no credential"; }
run_dir="$(keep_run "the fake run")"
grep -q 'cluster_type = "gpu-single"' "$run_dir/terraform/full-everything/terraform.tfvars" \
  || fail "the fake run didn't rewrite full-everything's cluster type to gpu-single"
grep -Eq 'instance_group += *true' "$run_dir/terraform/full-everything/terraform.tfvars" \
  || fail "the fake run turned full-everything's instance_group off"
grep -q 'condition = self.cidr != ""' "$run_dir/terraform/full/main.tf" \
  || fail "the fake run dropped full/'s subnet pool check, which only a kind run drops"

# --- UPGRADE_REAL=1 refuses before it builds anything ------------------------

refuses() {
  local what="$1" want="$2"; shift 2
  : > "$work/go.log"
  fails_with "$what" "$want" upgrade UPGRADE_REAL=1 "$@"
  [ ! -s "$work/go.log" ] || fail "$what: the check built something before refusing"
}
refuses "no endpoint" "UPGRADE_REAL=1 needs K0RDENT_ENDPOINT, the endpoint to apply against"
refuses "no region" "K0RDENT_REGION is not set, which the walkthroughs build in" \
  K0RDENT_ENDPOINT=http://localhost:14000
refuses "no project" "K0RDENT_PROJECT is not set, which the walkthroughs build in" \
  K0RDENT_ENDPOINT=http://localhost:14000 K0RDENT_REGION=r
refuses "kind with no org client id" "REAL_API_ORG_CLIENT_ID is not set. The env file make real-api-stack writes sets it" \
  K0RDENT_ENDPOINT=http://localhost:14000 K0RDENT_REGION=r K0RDENT_PROJECT=p
refuses "hardware with no confirm" "UPGRADE_REAL_CONFIRM=api.example.com make upgrade-test-real" \
  K0RDENT_ENDPOINT=https://api.example.com K0RDENT_REGION=r K0RDENT_PROJECT=p \
  K0RDENT_CLIENT_ID=id K0RDENT_CLIENT_SECRET=secret REAL_API_CLUSTER_TYPE=ct
refuses "hardware with another host confirmed" "They have to match" \
  K0RDENT_ENDPOINT=https://api.example.com UPGRADE_REAL_CONFIRM=api.example.org \
  K0RDENT_REGION=r K0RDENT_PROJECT=p K0RDENT_CLIENT_ID=id K0RDENT_CLIENT_SECRET=secret REAL_API_CLUSTER_TYPE=ct
refuses "hardware over http" "Use https, or the credential and every request go to it in the clear" \
  K0RDENT_ENDPOINT=http://api.example.com UPGRADE_REAL_CONFIRM=api.example.com \
  K0RDENT_REGION=r K0RDENT_PROJECT=p K0RDENT_CLIENT_ID=id K0RDENT_CLIENT_SECRET=secret REAL_API_CLUSTER_TYPE=ct
refuses "hardware with no client id" "K0RDENT_CLIENT_ID is not set, which the previous release signs in with" \
  K0RDENT_ENDPOINT=https://api.example.com UPGRADE_REAL_CONFIRM=api.example.com \
  K0RDENT_REGION=r K0RDENT_PROJECT=p K0RDENT_CLIENT_SECRET=secret REAL_API_CLUSTER_TYPE=ct
refuses "hardware with no cluster type" "REAL_API_CLUSTER_TYPE is not set. It is a cluster type id in" \
  K0RDENT_ENDPOINT=https://api.example.com UPGRADE_REAL_CONFIRM=api.example.com \
  K0RDENT_REGION=r K0RDENT_PROJECT=p K0RDENT_CLIENT_ID=id K0RDENT_CLIENT_SECRET=secret
: > "$work/go.log"
fails_with "a kind stack stack-check refuses" "the kind stack isn't ready to upgrade from" \
  kind_run STUB_STACK_CHECK=dirty
[ ! -s "$work/go.log" ] || fail "a refused kind stack: the check built something before refusing"

# --- a kind run ---------------------------------------------------------------

: > "$work/tf.log"; : > "$work/go.log"; : > "$work/stack.log"
passes "the kind run" kind_run
want "the kind run" "landscape: kind"
want "the kind run" "State from 0.1.0 plans empty under this tree, but for accepted changes."
want "the kind run" "6 of 9 registered resource types covered"
want "the kind run" "not covered, accepted: k0rdent_instance_group"
want "the kind run" "accepted: k0rdent_kubernetes_cluster.this drift"
no_want "the kind run" "k0rdent_instance_group.this\[0\] drift"
[ "$(grep -c stack-check "$work/stack.log")" = 1 ] || fail "the kind run didn't run stack-check once"
grep -q "^previous apply pick client=org-client poll=200ms" "$work/tf.log" \
  || fail "the kind run didn't run the walkthroughs as the organization's account at 200ms polls"
grep -q "^candidate destroy full " "$work/tf.log" \
  || fail "the kind run didn't destroy with the candidate"
run_dir="$(keep_run "the kind run")"
for name in full full-everything; do
  grep -q 'condition = self.cidr != ""' "$run_dir/terraform/$name/main.tf" \
    && fail "the kind run kept full/'s subnet pool check in $name"
  for switch in instance_group file_store vpc_peering; do
    grep -Eq "^  $switch += *false" "$run_dir/terraform/$name/terraform.tfvars" \
      || fail "the kind run kept $name's $switch on"
  done
done
grep -Eq '^  webhook += *true' "$run_dir/terraform/full-everything/terraform.tfvars" \
  || fail "the kind run turned full-everything's webhook off, which a kind stack serves"
grep -q 'cluster_type = "gpu-single"' "$run_dir/terraform/full-everything/terraform.tfvars" \
  && fail "the kind run rewrote full-everything's cluster type to gpu-single, which only the fake does"

# --- a hardware run -----------------------------------------------------------

: > "$work/tf.log"; : > "$work/go.log"; : > "$work/stack.log"
passes "the hardware run" hw_run
want "the hardware run" "landscape: hardware"
want "the hardware run" "State from 0.1.0 plans empty under this tree, but for accepted changes."
want "the hardware run" "7 of 9 registered resource types covered"
want "the hardware run" "not covered, accepted: k0rdent_project"
want "the hardware run" "not covered, accepted: k0rdent_webhook"
grep -q "^previous apply pick client=hw-client poll=default" "$work/tf.log" \
  || fail "the hardware run didn't use the landscape's credential at the default poll interval"
run_dir="$(keep_run "the hardware run")"
[ ! -d "$run_dir/terraform/cluster-own-project" ] || fail "the hardware run ran cluster-own-project"
grep -q 'address_pool = "pool-7"' "$run_dir/terraform/full/terraform.tfvars" \
  || fail "the hardware run didn't name full/'s address pool"
grep -q 'cluster_type = "ct-hw"' "$run_dir/terraform/full-everything/terraform.tfvars" \
  || fail "the hardware run didn't build the cluster type the runner named"
for name in full full-everything; do
  for switch in webhook own_project; do
    grep -Eq "^  $switch += *false" "$run_dir/terraform/$name/terraform.tfvars" \
      || fail "the hardware run kept $name's $switch on"
  done
done
for switch in instance_group file_store vpc_peering; do
  grep -Eq "^  $switch += *true" "$run_dir/terraform/full-everything/terraform.tfvars" \
    || fail "the hardware run turned full-everything's $switch off, which a landscape serves"
done
# One directory at a time: nothing applies while the one before it is up.
STUB_TF_LOG="$work/tf.log" python3 - <<'PY' || fail "the hardware run held more than one directory at a time"
import os, sys
alive = None
for line in open(os.environ["STUB_TF_LOG"]):
    p = line.split()
    if len(p) < 3 or p[1] not in ("apply", "destroy") or p[2] == "pick":
        continue
    if p[1] == "apply":
        if alive is not None and alive != p[2]:
            sys.exit("%s applied while %s was still up" % (p[2], alive))
        alive = p[2]
    else:
        alive = None
PY

# --- the accepted file's landscape rules --------------------------------------

saved="$work/accepted.saved"
cp "$repo/ci/upgrade-accepted.json" "$saved"
mutate() {  # mutate <python lines over f, the accepted file as a dict>
  cp "$saved" "$repo/ci/upgrade-accepted.json"
  ACCEPTED="$repo/ci/upgrade-accepted.json" MUTATION="$1" python3 - <<'PY'
import json, os
e = os.environ
f = json.load(open(e["ACCEPTED"]))
exec(e["MUTATION"])
json.dump(f, open(e["ACCEPTED"], "w"), indent=2)
PY
}

mutate 'f["changes"] = [e for e in f["changes"] if e["directory"] != "cluster"]'
fails_with "the cluster drift with no entry" "NOT ACCEPTED: k0rdent_kubernetes_cluster.walkthrough drift" fake_run

mutate 'f["not_covered"] = [e for e in f["not_covered"] if e["type"] != "k0rdent_instance_group"]'
fails_with "kind without its instance_group entry" "k0rdent_instance_group is in no walkthrough directory's state" kind_run

mutate 'f["not_covered"].append({"type": "k0rdent_webhook", "reason": "to make it stale"})'
fails_with "a landscapeless entry for a covered type" "stale not_covered entry: k0rdent_webhook is covered" fake_run

mutate 'f["changes"].append({"from": "0.1.0", "directory": "network", "change": "k0rdent_network.this drift", "landscape": "kind", "reason": "nothing matches this"})'
fails_with "a kind change nothing matches" "stale accepted change: network k0rdent_network.this drift" kind_run

mutate 'f["changes"][0]["landscape"] = "mars"'
fails_with "an unknown landscape" "names landscape mars, and the landscapes are fake, kind and hardware" fake_run

cp "$saved" "$repo/ci/upgrade-accepted.json"
passes "the fake run again, with the accepted file back" fake_run

echo "upgrade-against-fake test: ok"
