# shellcheck shell=bash
#
# What the ci/ scripts that run examples/walkthrough against the fake share:
# which directories the fake can apply, the values look/ picks for them, and
# each directory's terraform.tfvars filled in with those values. Sourced after
# common.sh, never run:
#
#   . "$REPO/scripts/lib/common.sh"
#   . "$REPO/scripts/lib/walkthrough.sh"
#
# A new variable in a walkthrough's terraform.tfvars is added to
# walkthrough_tfvars here, once, for every script. scripts/tests/test-script-lib.sh
# tests it.

if [ -n "${K0RDENT_WALKTHROUGH_LIB:-}" ]; then
  return 0
fi
K0RDENT_WALKTHROUGH_LIB=1

# nodes/ needs a real cluster's API server, which the fake doesn't run.
WALKTHROUGH_SKIP="nodes"

# Set by walkthrough_pick and walkthrough_secrets, read by walkthrough_tfvars.
# shellcheck disable=SC2034 # read by the scripts that source this
region=""
cluster_type=""
filtered_type=""
organization=""
project=""
WALKTHROUGH_SSH_KEY=""

# walkthrough_dirs <examples dir> prints, one per line, every directory under
# <examples dir> with a main.tf that the fake can apply, look first, then the
# rest in order. full-everything follows full: full/ again with every switch in
# its build block on.
walkthrough_dirs() {
  local d name rest=""
  for d in "$1"/*; do
    name="${d##*/}"
    [ -f "$d/main.tf" ] || continue
    case " $WALKTHROUGH_SKIP " in *" $name "*) continue ;; esac
    if [ "$name" = look ]; then
      echo look
    else
      rest="$rest $name"
    fi
  done
  for name in $rest; do
    echo "$name"
    [ "$name" = full ] && echo full-everything
  done
  return 0
}

# walkthrough_pick <file> sets region, cluster_type, filtered_type,
# organization and project from look/'s `output -json`, as a reader picks
# them from what look printed.
walkthrough_pick() {
  local picked
  picked="$(walkthrough_picked "$1")" || die "could not pick values from look's output in $1"
  read -r region cluster_type filtered_type organization project <<< "$picked"
}

# walkthrough_picked <file> prints what walkthrough_pick sets. It's its own
# function because bash 3.2 misreads a quote in a here-document inside $(...).
walkthrough_picked() {
  python3 - "$1" <<'PY'
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
}

# walkthrough_secrets <run dir> makes what a reader brings and a runner
# doesn't have: an SSH key for the nodes, at WALKTHROUGH_SSH_KEY, and the
# webhook secret full/ reads from TF_VAR_webhook_secret.
walkthrough_secrets() {
  ssh-keygen -q -t ed25519 -N '' -C walkthrough@ci -f "$1/id_ed25519" || die "could not make an SSH key"
  WALKTHROUGH_SSH_KEY="$1/id_ed25519.pub"
  TF_VAR_webhook_secret="$(python3 -c 'import secrets; print(secrets.token_hex(32))')"
  export TF_VAR_webhook_secret
}

# walkthrough_tfvars <name> <examples dir> <prefix> prints <name>'s
# terraform.tfvars, the shipped one with every value a reader fills in set:
# each top-level key below is replaced, and every other line kept. A key the
# file leaves empty that isn't below fails, naming it, so a new variable can't
# go unfilled. full-everything is full's file with every build switch on.
walkthrough_tfvars() {
  local name="$1" src="$1"
  [ "$name" = full-everything ] && src=full
  [ -f "$2/$src/main.tf" ] || { echo "walkthrough: no directory $src in $2" >&2; return 1; }
  [ -f "$2/$src/terraform.tfvars" ] || return 0
  WT_NAME="$name" WT_PREFIX="$3" WT_REGION="$region" WT_PROJECT="$project" \
    WT_ORGANIZATION="$organization" WT_CLUSTER_TYPE="$cluster_type" \
    WT_FILTERED_TYPE="$filtered_type" WT_SSH_KEY="$WALKTHROUGH_SSH_KEY" \
    python3 - "$2/$src/terraform.tfvars" <<'PY'
import json, os, re, sys
e = os.environ
name = e["WT_NAME"]
full = name in ("full", "full-everything")
values = {
    "prefix": e["WT_PREFIX"],
    "region": e["WT_REGION"],
    "project": e["WT_PROJECT"],
    "organization": e["WT_ORGANIZATION"],
    "cluster_type": e["WT_FILTERED_TYPE"] if full else e["WT_CLUSTER_TYPE"],
    "ssh_public_key_file": e["WT_SSH_KEY"],
    "allowed_cidr": "203.0.113.0/24",
    "allowed_cidrs": ["203.0.113.0/24", "198.51.100.0/25"],
    "webhook_url": "https://hooks.example.com/k0rdent",
}
out, missing, in_build = [], [], False
for line in open(sys.argv[1]).read().splitlines():
    m = re.match(r'^([a-z_]+)\s*=\s*(.*?)\s*(#.*)?$', line)
    if m and m.group(1) in values:
        v = values[m.group(1)]
        if v == "":
            missing.append(m.group(1) + " (look gave no value)")
        line = "%s = %s" % (m.group(1), json.dumps(v))
    elif m and m.group(2) in ('""', "[]", "{}"):
        missing.append(m.group(1))
    if line.startswith("build = {"):
        in_build = True
    elif line.startswith("}"):
        in_build = False
    elif in_build and name == "full-everything":
        line = re.sub(r"=(\s*)false\b", r"=\1true ", line)
    out.append(line)
if missing:
    sys.exit("walkthrough: %s/terraform.tfvars needs %s, which walkthrough_tfvars in scripts/lib/walkthrough.sh doesn't fill in"
             % (name, ", ".join(missing)))
print("\n".join(out))
PY
}
