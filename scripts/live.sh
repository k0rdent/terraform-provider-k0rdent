#!/usr/bin/env bash
#
# The same lifecycle `scripts/demo.sh` runs, against a REAL control plane.
#
#   scripts/live.sh preflight   read-only. Mint, list regions, list cluster types
#   scripts/live.sh run         the full lifecycle. CREATES REAL RESOURCES
#
# THIS IS NOT THE DEMO. There is no fake here. `run` creates a cluster on
# whatever landscape K0RDENT_ENDPOINT names, and destroys it again. It
# refuses to start unless LIVE_CONFIRM names that endpoint's host, and it
# always attempts a destroy on the way out, including after a failure.
#
# CONFIGURATION — all from the environment, the same names the provider reads:
#
#   K0RDENT_ENDPOINT       required. Base URL, scheme and host only, no /v1
#   K0RDENT_REGION         required. A deployable region id; `preflight` lists them
#   K0RDENT_PROJECT        the project's id. Not needed when the run makes its
#                          own project or names one with TF_VAR_project_id
#
#   TF_VAR_organization_id   the run creates its own project in this organization
#                            and deletes it with everything else. Needs an
#                            organization-scoped credential. `preflight` lists
#                            the organizations the credential can see
#   TF_VAR_project_id        an existing project to build in, or with
#                            TF_VAR_organization_id the new project's id
#
#   K0RDENT_CLIENT_ID      service account — the supported credential
#   K0RDENT_CLIENT_SECRET
#     or
#   K0RDENT_API_KEY        API key. Works, and the provider warns about it
#
#   LIVE_CONFIRM             `run` only. Must equal the endpoint's host
#   LIVE_CLUSTER             cluster id. Default: tf-live-<epoch>, unique per run
#   LIVE_KEEP=1              skip the destroy, to inspect what was made. The
#                            variables the cluster ended on go to
#                            .live/kept.tfvars, for `-var-file`
#   LIVE_UPGRADE=1           run the upgrade step on a landscape that is not local.
#                            Default: on for localhost (a kind stack), off
#                            elsewhere. It needs two or more `available` versions
#
# WHAT THIS PROVES
#   Everything `make demo` proves, minus the fake's assumptions — because there
#   is no fake. The measurement is the same one: apply, then plan, and the plan
#   must report no changes. Against a real server that is a claim about the
#   real server.
#
# PACING
#   A credential is allowed ten token mints per sixty seconds, and every
#   Terraform command is a fresh provider process that mints once. Running the
#   lifecycle back to back exhausts that budget and the run dies on a 429 that
#   looks like a credential problem. So this script waits between steps. That
#   is why it is slower than it looks like it should be.

set -uo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"
LIVE_DIR="$REPO/examples/live"
RUN_DIR="$REPO/.live"
RESULTS="$RUN_DIR/results.txt"
KEPT_VARS="$RUN_DIR/kept.tfvars"

# shellcheck source=lib/common.sh
. "$REPO/scripts/lib/common.sh"

# record <STATUS> <step> <detail>
record() {
  private_dir "$RUN_DIR"
  printf '%-5s %-22s %s\n' "$1" "$2" "${3:-}" >> "$RESULTS"
}

# Mint budget: ten per sixty seconds per credential. Six seconds between
# Terraform commands keeps a long run under it without stretching the wall
# clock more than it has to.
MINT_PACE="${LIVE_MINT_PACE:-6}"
pace() { sleep "$MINT_PACE"; }

# build_live_provider builds the provider the way scripts/demo.sh does, and
# points this shell's Terraform at it.
build_live_provider() {
  private_dir "$RUN_DIR"
  build_provider "$RUN_DIR" 0.0.0-live
  export TF_CLI_CONFIG_FILE="$RUN_DIR/terraform.rc"
  export TF_IN_AUTOMATION=1
}

tf() { ( cd "$LIVE_DIR" && "$TF_BIN" "$@" ); }

# write_kept_vars records the variables a kept cluster ended on: its id, the
# worker count after the scale, and the cluster type and version the upgrade
# pinned. A command run later without them plans the cluster back to where the
# run started, or onto whichever type sorts first in the catalog by then.
#
# It runs from the EXIT trap too, so a failure here warns and returns non-zero
# rather than exiting: the trap still has the destroy to record and the
# results to print. private_dir runs in a subshell for that reason, since it
# dies on a bad directory.
write_kept_vars() {
  ( private_dir "$RUN_DIR" ) || return 1
  {
    printf 'cluster_name = "%s"\n' "$CLUSTER"
    [ -z "${WORKERS:-}" ] || printf 'worker_nodes = %s\n' "$WORKERS"
    [ -z "${TF_VAR_cluster_type_id:-}" ] \
      || printf 'cluster_type_id = "%s"\n' "$TF_VAR_cluster_type_id"
    [ -z "${TF_VAR_cluster_type_version:-}" ] \
      || printf 'cluster_type_version = "%s"\n' "$TF_VAR_cluster_type_version"
    [ -z "${TF_VAR_organization_id:-}" ] \
      || printf 'organization_id = "%s"\n' "$TF_VAR_organization_id"
    [ -z "${TF_VAR_project_id:-}" ] \
      || printf 'project_id = "%s"\n' "$TF_VAR_project_id"
  } > "$KEPT_VARS" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

read_config() {
  ENDPOINT="$(eval printf '%s' "\"\${${ENV_PREFIX}ENDPOINT:-}\"")"
  REGION="$(eval printf '%s' "\"\${${ENV_PREFIX}REGION:-}\"")"
  PROJECT="$(eval printf '%s' "\"\${${ENV_PREFIX}PROJECT:-}\"")"
  CLIENT_ID="$(eval printf '%s' "\"\${${ENV_PREFIX}CLIENT_ID:-}\"")"
  CLIENT_SECRET="$(eval printf '%s' "\"\${${ENV_PREFIX}CLIENT_SECRET:-}\"")"
  API_KEY="$(eval printf '%s' "\"\${${ENV_PREFIX}API_KEY:-}\"")"

  [ -n "$ENDPOINT" ] || die "${ENV_PREFIX}ENDPOINT is not set. It is the API base URL, scheme and host only — for example http://localhost:8000"
  endpoint_is_encrypted "$ENDPOINT" || die "${ENV_PREFIX}ENDPOINT is $ENDPOINT, and the credential goes in the body of the token request.

   Use https. http is accepted only for localhost or a loopback IP, which is
   where a kind stack and the local fake run. The provider refuses the same."
  # The preflight is how a reader finds the region and project ids, so it runs
  # without them and stops where they're needed. The lifecycle needs both.
  [ -n "$REGION" ] || [ "${1:-}" = preflight ] \
    || die "${ENV_PREFIX}REGION is not set. Run 'make live-preflight' to list the region ids this landscape serves."
  NEW_IN_ORG="${TF_VAR_organization_id:-}"
  NAMED_PROJECT="${TF_VAR_project_id:-}"
  if [ -n "$NEW_IN_ORG" ]; then
    PROJECT_SAYS="a new one in $NEW_IN_ORG${NAMED_PROJECT:+, named $NAMED_PROJECT}, deleted at the end"
  elif [ -n "$NAMED_PROJECT" ]; then
    PROJECT_SAYS="$NAMED_PROJECT"
  elif [ -n "$PROJECT" ]; then
    PROJECT_SAYS="$PROJECT"
  elif [ "${1:-}" = preflight ]; then
    PROJECT_SAYS="not chosen yet"
  else
    die "no project. Pick one of:

   ${ENV_PREFIX}PROJECT=<id>              build in an existing project
   TF_VAR_organization_id=<org id>   let the run make its own project and delete it

   'make live-preflight' lists the organizations and projects this credential can see."
  fi

  if [ -n "$CLIENT_ID" ] || [ -n "$CLIENT_SECRET" ]; then
    [ -n "$CLIENT_ID" ] && [ -n "$CLIENT_SECRET" ] \
      || die "the client_credentials grant needs both ${ENV_PREFIX}CLIENT_ID and ${ENV_PREFIX}CLIENT_SECRET"
    [ -z "$API_KEY" ] \
      || die "both a service account and an API key are set, and they are different identities. Unset whichever you did not mean to use."
    CRED_KIND="service account"
  elif [ -n "$API_KEY" ]; then
    CRED_KIND="API key"
  else
    die "no credential. Set ${ENV_PREFIX}CLIENT_ID and ${ENV_PREFIX}CLIENT_SECRET for a service account, or ${ENV_PREFIX}API_KEY for an API key."
  fi

  HOST="$(endpoint_host "$ENDPOINT")"
}

# mint_token prints an access token, or exits. python3 rather than jq: jq is a
# tier-2 tool and this has to work wherever the provider does, and python3 is
# already required by ci/plan-against-fake.sh.
mint_token() {
  local body
  body="$(token_request_body "$CLIENT_ID" "$CLIENT_SECRET" "$API_KEY")"

  local resp
  resp="$(printf '%s' "$body" | curl -sS -m 30 -X POST \
    "$ENDPOINT/v1/regions/global/auth/token" \
    -H 'Content-Type: application/json' --data-binary @- 2>&1)" \
    || die "could not reach the token endpoint at $ENDPOINT"

  RESP="$resp" python3 -c '
import json, os, sys
raw = os.environ["RESP"]
try:
    d = json.loads(raw)
except Exception:
    sys.stderr.write("the token endpoint did not return JSON:\n" + raw[:400] + "\n")
    sys.exit(1)
tok = d.get("access_token")
if not tok:
    # Two error envelopes exist: the contract shape and the gateway bare one.
    err = d.get("error")
    msg = err.get("message") if isinstance(err, dict) else (d.get("message") or err)
    sys.stderr.write("the mint failed: %s\n" % (msg or raw[:400]))
    sys.exit(1)
print(tok)
'
}

# api_get <path> prints the response body for an authenticated GET. The bearer
# reaches curl on stdin, never in argv.
api_get() { curl_bearer "$TOKEN" -sS -m 30 "$ENDPOINT$1"; }

# list_all <path> <key> prints every row of an offset-paged list, one JSON
# object per line. It follows the pages because a server may answer with fewer
# rows than asked for, and the first page alone would read as the whole list.
list_all() {
  local offset=0 page rows
  for _ in $(seq 1 50); do
    page="$(api_get "$1?limit=100&offset=$offset")" || return 1
    rows="$(PAGE="$page" KEY="$2" python3 -c '
import json, os
for r in json.loads(os.environ["PAGE"]).get(os.environ["KEY"]) or []:
    print(json.dumps(r))
')" || return 1
    [ -n "$rows" ] || return 0
    printf '%s\n' "$rows"
    offset=$((offset + $(printf '%s\n' "$rows" | wc -l)))
  done
}

# ---------------------------------------------------------------------------
# preflight — read-only, safe against any landscape
# ---------------------------------------------------------------------------

cmd_preflight() {
  read_names; find_terraform; read_config preflight

  printf '%sk0rdent AI Terraform provider — live preflight%s\n' "$B" "$R"
  note "endpoint: $ENDPOINT"
  note "region:   ${REGION:-not chosen yet}"
  note "project:  $PROJECT_SAYS"
  note "credential: $CRED_KIND"
  note "read-only: this creates nothing"

  step "1/5  Mint an access token"
  TOKEN="$(mint_token)" || die "the credential could not mint a token.

   A 401 means a wrong client id, secret or API key. Prefer a service account.
   A 429 means token requests are rate limited; wait a minute."
  good "minted an access token"

  step "2/5  GET /v1/regions — the region ids this landscape serves"
  api_get /v1/regions | python3 -c '
import json, sys
d = json.load(sys.stdin)
rs = d.get("regions") or []
if not rs:
    print("   (none advertised)")
for r in rs:
    print("   %-16s %s" % (r.get("id", "?"), r.get("type", "")))
' || die "could not list regions"
  note 'the `region` attribute takes one whose type is "regional". "global" is the'
  note 'control-plane tier — the provider addresses it on its own and rejects it here.'

  step "3/5  The organizations and projects this credential can see"
  list_all /v1/regions/global/organizations organizations | python3 -c '
import json, sys
rows = [json.loads(l) for l in sys.stdin if l.strip()]
print("   %d organization(s)" % len(rows))
for o in rows[:10]:
    print("   %-28s %s" % (o.get("id", "?"), o.get("status", "")))
' || warn "could not list organizations"
  list_all /v1/regions/global/projects projects | python3 -c '
import json, sys
rows = [json.loads(l) for l in sys.stdin if l.strip()]
print("   %d project(s)" % len(rows))
for p in rows[:10]:
    print("   %-28s org %-20s region %-14s %s" % (p.get("id", "?"), p.get("organizationId", "?"), p.get("region") or "-", p.get("status", "")))
' || warn "could not list projects"
  note "K0RDENT_PROJECT takes a project id from this list. TF_VAR_organization_id takes an"
  note "organization id instead, and the run makes and deletes its own project there."

  if [ -z "$REGION" ]; then
    printf '\n%s%sPREFLIGHT PASS so far%s. The credential works. Pick a region whose type is\n' "$GREEN" "$B" "$R"
    printf '"regional" from the list above and run it again:\n\n'
    printf '   %sexport %sREGION=<a regional id>%s\n   %smake live-preflight%s\n\n' "$B" "$ENV_PREFIX" "$R" "$B" "$R"
    return 0
  fi

  step "4/5  The cluster-type catalog for $REGION"
  api_get "/v1/regions/$REGION/compute/cluster-types" | CT_REGION="$REGION" python3 -c '
import json, os, sys
d = json.load(sys.stdin)
cts = d.get("clusterTypes") or []
print("   %d cluster type(s)" % len(cts))
for c in cts[:10]:
    print("   %-28s %s" % (c.get("id", "?"), c.get("status", "")))
if not cts:
    print("")
    print("   An empty catalog is a PASS for this preflight — the call succeeded and")
    print("   the credential was accepted. It does mean `make live` has nothing to")
    print("   build a cluster from. See TESTING.md, \"Seed a cluster type\".")
' || die "could not list cluster types for region $REGION"

  if [ "$PROJECT_SAYS" = "not chosen yet" ]; then
    printf '\n%s%sPREFLIGHT PASS so far%s. Nothing was created. The plan needs a project, so pick\n' "$GREEN" "$B" "$R"
    printf 'one from the lists above and run it again:\n\n'
    printf '   %sexport TF_VAR_organization_id=<org id>%s   the run makes its own project\n' "$B" "$R"
    printf '   %sexport TF_VAR_project_id=<project id>%s    or builds in one you have\n\n' "$B" "$R"
    return 0
  fi

  step "5/5  terraform plan — the provider's own read path"
  note "a plan reads the data sources and proposes a create. It applies nothing."
  pace
  rm -rf "$LIVE_DIR/.terraform" "$LIVE_DIR/.terraform.lock.hcl"
  warn "the yellow \"Provider development overrides are in effect\" line below is the"
  warn "override working, not a failure. Do not run \`$TF_BIN init\`."
  build_live_provider
  set +e
  tf plan -detailed-exitcode -input=false -no-color -var cluster_name="${LIVE_CLUSTER:-tf-live-preflight}"
  local status=$?
  set -e
  case "$status" in
    0) good "plan succeeded and proposes nothing" ;;
    2) good "plan succeeded and proposes a create — expected, nothing was applied" ;;
    *) die "terraform plan failed with exit $status" ;;
  esac

  printf '\n%s%sPREFLIGHT PASS%s — against %s. Nothing was created.\n' "$GREEN" "$B" "$R" "$HOST"
  printf '\nThe lifecycle is: %sLIVE_CONFIRM=%s make live%s\n\n' "$B" "$HOST" "$R"
}

# ---------------------------------------------------------------------------
# run — the lifecycle. Creates real resources.
# ---------------------------------------------------------------------------

cmd_run() {
  read_names; find_terraform; read_config

  [ -n "${LIVE_CONFIRM:-}" ] || die "make live creates REAL resources on $ENDPOINT.

   Confirm the landscape by naming its host:

       LIVE_CONFIRM=$HOST make live

   Use 'make live-preflight' first — it is read-only and tells you whether the
   credential, the region and the catalog are usable."

  [ "$LIVE_CONFIRM" = "$HOST" ] || die "LIVE_CONFIRM is \"$LIVE_CONFIRM\" but ${ENV_PREFIX}ENDPOINT's host is \"$HOST\".

   They have to match. This guard exists so that a shell still holding a
   production endpoint cannot run a create because the command was in history."

  CLUSTER="${LIVE_CLUSTER:-tf-live-$(date +%s)}"

  private_dir "$RUN_DIR"
  : > "$RESULTS"
  {
    printf 'k0rdent AI Terraform provider — live run\n'
    printf 'date:       %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf 'endpoint:   %s\n' "$ENDPOINT"
    printf 'region:     %s\n' "$REGION"
    printf 'project:    %s\n' "$PROJECT_SAYS"
    printf 'credential: %s\n' "$CRED_KIND"
    printf 'cluster:    %s\n' "$CLUSTER"
    printf '\n'
  } >> "$RESULTS"

  # Not `local`: the EXIT trap runs after this function returns.
  destroyed=0
  cleanup() {
    local rc=$?
    # Disarm first. Without this the INT/TERM handler's own exit re-enters the
    # EXIT trap and the destroy runs twice.
    trap - EXIT INT TERM
    if [ "$destroyed" = "1" ]; then
      :
    elif [ "${LIVE_KEEP:-0}" = "1" ]; then
      if write_kept_vars; then
        warn "LIVE_KEEP=1 — leaving $CLUSTER in place. Destroy it with:"
        warn "  cd $LIVE_DIR && $TF_BIN destroy -var-file=$KEPT_VARS"
      else
        warn "LIVE_KEEP=1 — leaving $CLUSTER in place, but $KEPT_VARS could not be written."
        warn "destroy it with: cd $LIVE_DIR && $TF_BIN destroy -var cluster_name=$CLUSTER"
      fi
      record SKIP destroy "LIVE_KEEP=1"
    else
      printf '\n%s━━ Cleaning up %s\n' "$B" "$R"
      note "attempting a destroy even though the run did not finish"
      pace
      if tf destroy -auto-approve -input=false -no-color \
           -var cluster_name="$CLUSTER" ${WORKERS:+-var worker_nodes="$WORKERS"} >/dev/null 2>&1; then
        good "destroyed $CLUSTER"
        record PASS destroy "on cleanup"
        destroyed=1
      else
        warn "the cleanup destroy FAILED. $CLUSTER may still exist on $HOST."
        warn "check it by hand: cd $LIVE_DIR && $TF_BIN destroy -var cluster_name=$CLUSTER"
        record FAIL destroy "cleanup destroy failed; $CLUSTER may still exist"
      fi
    fi
    # Terraform writes state next to the configuration, and that state describes
    # real resources. Once the destroy has run there is nothing left for it to
    # describe, and leaving it behind means the next `make check` name audit
    # reports terraform.tfstate.*.backup files as places a rename would have to
    # edit — which reads like a dirty tree. Keep it only when the resources were
    # kept.
    if [ "$destroyed" = "1" ] && [ "${LIVE_KEEP:-0}" != "1" ]; then
      rm -f "$LIVE_DIR/terraform.tfstate" "$LIVE_DIR"/terraform.tfstate.*.backup \
            "$LIVE_DIR/terraform.tfstate.backup"
      rm -rf "$LIVE_DIR/.terraform" "$LIVE_DIR/.terraform.lock.hcl"
    fi
    printf '\n%sResults: %s%s\n' "$DIM" "$RESULTS" "$R"
    exit $rc
  }
  # EXIT alone is not enough: bash runs an EXIT trap when the shell exits
  # normally, but a fatal signal terminates it without running one — and a
  # ctrl-C half way through is exactly when the destroy matters most. So the
  # signals get a handler that just exits, which then runs the one EXIT trap
  # with a conventional 128+signal status.
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  printf '%sk0rdent AI Terraform provider — LIVE run%s\n' "$B" "$R"
  note "runner: $($TF_BIN version | head -1),  $(go version | awk '{print $3}')"
  warn "this creates real resources on $HOST and destroys them again"
  note "cluster id: $CLUSTER"

  # -------------------------------------------------------------------------
  step "1/8  Build the provider"
  # -------------------------------------------------------------------------
  rm -rf "$LIVE_DIR/.terraform" "$LIVE_DIR/.terraform.lock.hcl" \
         "$LIVE_DIR/terraform.tfstate" "$LIVE_DIR/terraform.tfstate.backup" "$KEPT_VARS"
  build_live_provider
  warn "no \`$TF_BIN init\`: under dev_overrides there is nothing to initialise, and"
  warn "every command prints a yellow overrides warning. That is not a failure."
  record PASS build

  # Pick the versions to upgrade between, before the create. The configuration
  # names a version through TF_VAR_ variables, so no `tf` call below changes.
  # The create has to pin the OLDEST available version: the unversioned head
  # reference binds the latest, and then there is nothing to upgrade to.
  UPGRADE_FROM=""; UPGRADE_FROM_N=""; UPGRADE_TO=""; UPGRADE_TO_N=""; UPGRADE_SKIP=""
  # On by default against a kind stack, where builds are mocked. Anywhere else
  # it walks a real cluster through real versions, so it waits for LIVE_UPGRADE=1.
  case "$HOST" in
    localhost|localhost:*|127.0.0.1|127.0.0.1:*) upgrade_default=1 ;;
    *)                                           upgrade_default=0 ;;
  esac
  if [ "${LIVE_UPGRADE:-$upgrade_default}" = "0" ]; then
    UPGRADE_SKIP="LIVE_UPGRADE=0"
    [ -n "${LIVE_UPGRADE:-}" ] || UPGRADE_SKIP="off by default away from a local stack, set LIVE_UPGRADE=1"
    note "no upgrade this run: $UPGRADE_SKIP"
  else
    TOKEN="$(mint_token)" || die "the credential could not mint a token to read the version list"
    local picked
    # The same cluster type the configuration picks: TF_VAR_cluster_type_id, or
    # else the first id of the whole catalog sorted by id, as the
    # cluster_types data source sorts it. Then the oldest and newest of its
    # available versions. A list call that fails is a FAIL, not a quiet SKIP.
    picked="$(TOKEN="$TOKEN" BASE="$ENDPOINT/v1/regions/$REGION/compute/cluster-types" \
      CT="${TF_VAR_cluster_type_id:-}" python3 -c '
import json, os, sys, urllib.error, urllib.parse, urllib.request
# urllib follows a redirect and keeps the Authorization header on the new
# request, wherever Location points. The provider refuses to follow one
# (internal/provider/transport.go), and so does this.
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise urllib.error.HTTPError(req.full_url, code, "refusing to follow a redirect to " + newurl, headers, fp)
opener = urllib.request.build_opener(NoRedirect)
def get(url):
    req = urllib.request.Request(url, headers={"Authorization": "Bearer " + os.environ["TOKEN"]})
    try:
        with opener.open(req, timeout=30) as r:
            return json.load(r)
    except Exception as e:
        sys.stderr.write("GET %s failed: %s\n" % (url, e))
        sys.exit(1)
base, ct = os.environ["BASE"], os.environ["CT"]
if not ct:
    ids, token = [], ""
    for _ in range(100):
        d = get(base + ("?pageToken=" + urllib.parse.quote(token) if token else ""))
        ids += [c["id"] for c in d.get("clusterTypes") or []]
        token = d.get("nextPageToken") or ""
        if not token:
            break
    if not ids:
        print("-"); sys.exit(0)
    ct = sorted(ids)[0]
vs = sorted((v for v in get(base + "/" + ct + "/versions?status=available").get("versions") or []
             if v.get("status") == "available"), key=lambda v: v["version"])
if len(vs) >= 2:
    print(ct, vs[0]["versionId"], vs[0]["version"], vs[-1]["versionId"], vs[-1]["version"])
else:
    print(ct)
')" || { record FAIL upgrade "could not read the cluster type's versions"; die "could not read the cluster type's versions to plan the upgrade step"; }
    local ct
    read -r ct UPGRADE_FROM UPGRADE_FROM_N UPGRADE_TO UPGRADE_TO_N <<< "$picked"
    if [ -n "$UPGRADE_FROM" ]; then
      export TF_VAR_cluster_type_id="$ct"
      export TF_VAR_cluster_type_version="$UPGRADE_FROM"
      note "creating on $ct version $UPGRADE_FROM_N, to upgrade to version $UPGRADE_TO_N later"
    else
      UPGRADE_SKIP="$ct has fewer than two available versions"
      [ "$ct" != "-" ] || UPGRADE_SKIP="the catalog is empty"
      note "no upgrade this run: $UPGRADE_SKIP"
    fi
  fi

  # -------------------------------------------------------------------------
  step "2/8  terraform apply — create the cluster"
  # -------------------------------------------------------------------------
  # Start the worker pool as LOW as it goes, so the scale step below has something it can
  # actually do. Resizing a pool that already holds nodes needs a node
  # reservation, and a landscape with no server inventory -- a kind stack, for
  # one -- cannot make one; growing a pool from zero is a supported path that
  # needs none. Starting at zero therefore exercises the scale leg everywhere
  # rather than skipping it wherever inventory is thin.
  #
  # That is zero unless the cluster type sets a floor, and the configuration
  # works it out from the pool, so the count is read back after the apply.
  #
  # Set LIVE_WORKERS=1 or more against a landscape with real inventory to
  # exercise the reservation path instead.
  pace
  if tf apply -auto-approve -input=false -no-color \
       -var cluster_name="$CLUSTER" ${LIVE_WORKERS:+-var worker_nodes="$LIVE_WORKERS"}; then
    WORKERS="$(tf output -raw worker_nodes)"
    good "applied"
    record PASS apply "cluster $CLUSTER, $WORKERS worker(s)"
  else
    record FAIL apply "terraform apply failed"
    die "terraform apply failed"
  fi

  # -------------------------------------------------------------------------
  step "3/8  terraform plan — THE MEASUREMENT"
  # -------------------------------------------------------------------------
  printf '%s   Everything else is setup. This is the claim: a plan straight after an\n' "$DIM"
  printf '   apply, with the configuration untouched, must report no changes. Exit 0\n'
  printf '   means no changes, 2 means changes, 1 means error.%s\n' "$R"
  pace
  set +e
  tf plan -detailed-exitcode -input=false -no-color \
    -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS"
  plan_status=$?
  set -e
  case "$plan_status" in
    0) good "exit 0 — no changes. The round trip held against a real server."
       record PASS replan "exit 0, no changes" ;;
    2) record FAIL replan "exit 2, plan proposes changes after a no-op apply"
       die "exit 2 — the plan proposes changes after an apply that changed nothing.
   That is a real finding. Read the diff above: if the provider is wrong, fix the
   provider; if the contract is wrong, file it as a spec finding." ;;
    *) record FAIL replan "terraform plan exit $plan_status"
       die "terraform plan failed with exit $plan_status" ;;
  esac

  # -------------------------------------------------------------------------
  step "4/8  The round trip, made visible"
  # -------------------------------------------------------------------------
  tf output -no-color cluster_type_round_trip
  note "if those two strings differ and the plan above was still clean, the provider"
  note "is tolerating the platform's version resolution rather than proposing to"
  note "change an immutable field forever. That is the single most valuable result."

  # -------------------------------------------------------------------------
  step "5/8  Scale the cluster, then plan again"
  # -------------------------------------------------------------------------
  scaled=$(( WORKERS + 1 ))
  note "worker pool $WORKERS -> $scaled nodes"
  pace
  scale_out="$RUN_DIR/scale.txt"
  set +e
  tf apply -auto-approve -input=false -no-color \
    -var cluster_name="$CLUSTER" -var worker_nodes="$scaled" > "$scale_out" 2>&1
  scale_status=$?
  set -e
  if [ "$scale_status" = "0" ]; then
    pace
    set +e
    tf plan -detailed-exitcode -input=false -no-color \
      -var cluster_name="$CLUSTER" -var worker_nodes="$scaled" >/dev/null
    plan_status=$?
    set -e
    if [ "$plan_status" = "0" ]; then
      good "scaled, and the plan is still clean"
      record PASS scale "$WORKERS -> $scaled, re-plan exit 0"
      WORKERS="$scaled"
    else
      record FAIL scale "re-plan after scale exit $plan_status"
      die "the plan after scaling proposes changes (exit $plan_status)"
    fi
  elif grep -qi 'reservation' "$scale_out"; then
    # Not a provider defect and not a pass. Some landscapes require a node
    # reservation before a pool that already has nodes can change size, and a
    # bare kind stack has no server inventory to reserve from. Say so rather
    # than failing the whole run over the environment.
    warn "SKIPPED — the platform requires a node reservation to resize this pool:"
    grep -i 'reservation' "$scale_out" | head -2 | sed 's/^/     /'
    note "that is an inventory limit of this landscape, not a provider result."
    note "full output: $scale_out"
    record SKIP scale "platform requires a node reservation"
  else
    grep -iE '^Error|error:' "$scale_out" | head -3 | sed 's/^/     /'
    record FAIL scale "scale apply failed, see $scale_out"
    die "the scale apply failed. Full output: $scale_out"
  fi

  # -------------------------------------------------------------------------
  step "6/8  Upgrade in place, then plan again"
  # -------------------------------------------------------------------------
  # The create pinned the oldest available version. Pinning the newest moves
  # the cluster forward in place, one version per step, and must not replace it.
  if [ -n "$UPGRADE_SKIP" ]; then
    warn "SKIPPED: $UPGRADE_SKIP"
    record SKIP upgrade "$UPGRADE_SKIP"
  else
    export TF_VAR_cluster_type_version="$UPGRADE_TO"
    note "version $UPGRADE_FROM_N -> $UPGRADE_TO_N"
    pace
    upgrade_plan="$RUN_DIR/upgrade-plan.txt"
    set +e
    tf plan -input=false -no-color \
      -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" > "$upgrade_plan" 2>&1
    plan_status=$?
    set -e
    if [ "$plan_status" != "0" ]; then
      grep -iE '^Error|error:' "$upgrade_plan" | head -3 | sed 's/^/     /'
      record FAIL upgrade "the upgrade plan failed, see $upgrade_plan"
      die "the upgrade plan failed. Full output: $upgrade_plan"
    fi
    if grep -qE 'must be replaced|forces replacement|destroy and then create' "$upgrade_plan"; then
      record FAIL upgrade "the upgrade plan proposes a REPLACEMENT"
      die "the upgrade plan proposes REPLACING the cluster. Full output: $upgrade_plan"
    fi
    steps_msg="$(grep -o 'This plan upgrades the cluster in [0-9]* [a-z]*' "$upgrade_plan" | head -1 || true)"
    [ -n "$steps_msg" ] && note "$steps_msg"
    pace
    upgrade_out="$RUN_DIR/upgrade.txt"
    set +e
    tf apply -auto-approve -input=false -no-color \
      -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" > "$upgrade_out" 2>&1
    apply_status=$?
    set -e
    if [ "$apply_status" != "0" ]; then
      grep -iE '^Error|error:' "$upgrade_out" | head -3 | sed 's/^/     /'
      record FAIL upgrade "the upgrade apply failed, see $upgrade_out"
      die "the upgrade apply failed. Full output: $upgrade_out"
    fi
    returned="$(tf output -no-color -json cluster_type_round_trip \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["returned"])')"
    case "$returned" in
      */versions/"$UPGRADE_TO") good "the platform now reports $returned" ;;
      *) record FAIL upgrade "after the apply the platform reports $returned, not version $UPGRADE_TO_N"
         die "the upgrade applied but the platform reports $returned, not .../versions/$UPGRADE_TO" ;;
    esac
    pace
    set +e
    tf plan -detailed-exitcode -input=false -no-color \
      -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" >/dev/null
    plan_status=$?
    set -e
    if [ "$plan_status" = "0" ]; then
      good "upgraded, and the plan is still clean"
      record PASS upgrade "version $UPGRADE_FROM_N -> $UPGRADE_TO_N${steps_msg:+, ${steps_msg#This plan upgrades the cluster in }}, re-plan exit 0"
    else
      record FAIL upgrade "re-plan after upgrade exit $plan_status"
      die "the plan after upgrading proposes changes (exit $plan_status)"
    fi

    # The create pinned a version, so step 3 no longer showed the unversioned
    # reference meeting the platform's pinned answer. Show it here: move the
    # configuration to the unversioned reference, which binds the latest
    # version the cluster is now on. That is a change to `cluster_type` as
    # written, so it plans an in-place update. It must never plan a
    # replacement, and once applied the next plan must be clean.
    unset TF_VAR_cluster_type_version
    head_plan="$RUN_DIR/head-plan.txt"
    pace
    set +e
    tf plan -input=false -no-color \
      -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" > "$head_plan" 2>&1
    plan_status=$?
    set -e
    if [ "$plan_status" != "0" ]; then
      record FAIL head-ref "plan with the unversioned reference failed, see $head_plan"
      die "the plan with the unversioned reference failed. Full output: $head_plan"
    fi
    if grep -qE 'must be replaced|forces replacement|destroy and then create' "$head_plan"; then
      record FAIL head-ref "the unversioned reference plans a REPLACEMENT"
      die "moving to the unversioned reference plans REPLACING the cluster. Full output: $head_plan"
    fi
    pace
    tf apply -auto-approve -input=false -no-color \
      -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" >/dev/null \
      || { record FAIL head-ref "apply with the unversioned reference failed"; die "the apply with the unversioned reference failed"; }
    pace
    set +e
    tf plan -detailed-exitcode -input=false -no-color \
      -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" >/dev/null
    plan_status=$?
    set -e
    if [ "$plan_status" = "0" ]; then
      tf output -no-color cluster_type_round_trip
      good "the unversioned reference holds against the platform's pinned answer"
      record PASS head-ref "in-place to the unversioned reference, re-plan exit 0"
    else
      record FAIL head-ref "re-plan with the unversioned reference exit $plan_status"
      die "the unversioned reference does not plan clean after its apply (exit $plan_status)"
    fi
    # Import below runs with the version pinned, which is worth covering too.
    export TF_VAR_cluster_type_version="$UPGRADE_TO"
  fi

  # -------------------------------------------------------------------------
  step "7/8  Import — drop the cluster from state and adopt it back"
  # -------------------------------------------------------------------------
  pace
  tf state rm -no-color "${PROVIDER_NAME}_kubernetes_cluster.live" >/dev/null \
    || { record FAIL import "state rm failed"; die "could not remove the cluster from state"; }
  note "removed from state; the cluster still exists on the platform"
  pace
  # Import resolves the project the way the provider block does, so a run that
  # builds somewhere else names its region and project in the id.
  run_project="$(tf output -json project | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"] or "")')"
  tf import -input=false -no-color \
    -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" \
    "${PROVIDER_NAME}_kubernetes_cluster.live" "${run_project:+$REGION/$run_project/}$CLUSTER" >/dev/null \
    || { record FAIL import "terraform import failed"; die "terraform import failed"; }
  good "imported — state rebuilt from the platform alone"

  pace
  import_plan="$RUN_DIR/import-plan.txt"
  set +e
  tf plan -detailed-exitcode -input=false -no-color \
    -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" > "$import_plan" 2>&1
  plan_status=$?
  set -e
  case "$plan_status" in
    0) good "the plan after import is already clean"
       record PASS import "re-plan exit 0" ;;
    2)
      # The one outcome that must never pass. A plan that proposes destroying
      # and recreating a cluster nobody touched is the destructive wrong diff.
      if grep -qE 'must be replaced|forces replacement|destroy and then create' "$import_plan"; then
        # awk rather than `sed -n '/x/,+12p'`: the +N address form is GNU-only
        # and this script has to run on a Mac.
        awk '/must be replaced/ {show = 14} show-- > 0 { print "     " $0 }' "$import_plan"
        record FAIL import "plan after import proposes a REPLACEMENT"
        die "the plan after import proposes REPLACING a running resource.
   Nothing changed but the state's provenance, so this is a provider bug.
   Read internal/provider/clustertyperef.go."
      fi
      note "the plan proposes an in-place update, which is expected:"
      awk '/will be updated in-place/ { show = 1 }
           show { print "     " $0 }
           show && /^    }/ { exit }' "$import_plan"
      note "cluster_type moves from the platform's resolved reference to the head form"
      note "the configuration uses. It is an in-place update, NOT a replacement —"
      note "getting that wrong destroys a running cluster."
      pace
      tf apply -auto-approve -input=false -no-color \
        -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" >/dev/null \
        || { record FAIL import "reconciling apply failed"; die "the reconciling apply failed"; }
      pace
      set +e
      tf plan -detailed-exitcode -input=false -no-color \
        -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS" >/dev/null
      plan_status=$?
      set -e
      if [ "$plan_status" = "0" ]; then
        good "reconciled, and the plan is clean"
        record PASS import "in-place reconcile, then exit 0"
      else
        record FAIL import "plan after reconciling exit $plan_status"
        die "the plan after reconciling still proposes changes (exit $plan_status)"
      fi
      ;;
    *) record FAIL import "plan after import exit $plan_status"
       die "the plan after import failed with exit $plan_status" ;;
  esac

  # -------------------------------------------------------------------------
  step "8/8  terraform destroy"
  # -------------------------------------------------------------------------
  if [ "${LIVE_KEEP:-0}" = "1" ]; then
    if write_kept_vars; then
      warn "LIVE_KEEP=1 — not destroying $CLUSTER. Its variables are in $KEPT_VARS"
    else
      warn "LIVE_KEEP=1 — not destroying $CLUSTER, but $KEPT_VARS could not be written"
    fi
    record SKIP destroy "LIVE_KEEP=1"
    destroyed=1
  else
    pace
    if tf destroy -auto-approve -input=false -no-color \
         -var cluster_name="$CLUSTER" -var worker_nodes="$WORKERS"; then
      good "destroyed"
      record PASS destroy
      destroyed=1
    else
      record FAIL destroy "terraform destroy failed"
      die "terraform destroy failed. $CLUSTER may still exist on $HOST."
    fi
  fi

  step "Results"
  cat "$RESULTS"
  if grep -q '^FAIL' "$RESULTS"; then
    printf '\n%s%sFAIL%s — against %s. See the lines above.\n\n' "$RED" "$B" "$R" "$HOST"
    exit 1
  fi
  printf '\n%s%sPASS%s — against %s, a real control plane. No fake anywhere in this run.\n\n' \
    "$GREEN" "$B" "$R" "$HOST"
}

case "${1:-run}" in
  run)       cmd_run ;;
  preflight) cmd_preflight ;;
  *)         die "unknown command ${1}; use run or preflight" ;;
esac
