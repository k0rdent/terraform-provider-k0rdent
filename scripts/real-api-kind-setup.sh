#!/usr/bin/env bash
#
# Builds the local kind stack `make real-api-check` runs against, from the
# platform API's source, and writes the env file the check sources.
#
#   REAL_API_SOURCE=<checkout> scripts/real-api-kind-setup.sh
#
# It creates the kind cluster, loads the API's images, installs the API in mock
# mode with the in-cluster mock identity provider, starts the port-forwards,
# seeds what the check needs and issues its two service accounts. Every step
# reuses what's already there, so it also refreshes a stack after a reboot,
# which stops the port-forwards. It writes the env file on every run, keeping
# only credentials that still work. TESTING.md has the steps around it.
#
# Configuration, from the environment:
#
#   REAL_API_SOURCE       required. A checkout of the platform API's source at
#                         the version in ci/real-api/api-version. Its make
#                         targets build the cluster
#   REAL_API_KIND_CLUSTER the kind cluster's name. Default k0rdent-rc5, whose
#                         context, kind-k0rdent-rc5, is the one the check reads
#                         by default
#   REAL_API_PORT_OFFSET  added to every forwarded port, so the stack can run
#                         beside another. Default 6000: the gateway on
#                         localhost:14000
#   REAL_API_ENV_FILE     where the env file goes, mode 600. Default
#                         .live/real-api-kind.env, which git ignores
#   REAL_API_FRESH=1      delete the cluster first and build it again, which
#                         clears whatever an interrupted run left on the stack.
#                         Only a cluster this script made: one it installed
#                         into with REAL_API_KIND_REUSE=1 is refused
#   REAL_API_KIND_REUSE=1 install into an existing kind cluster this script
#                         didn't make and that runs no platform API. Without
#                         it, the script refuses such a cluster
#
# It needs kind, kubectl, helm, docker, go, jq, curl, python3, make and git,
# and docker logged in to ghcr.io with an account that can read the API's
# images, which are private.
#
# Status: 0 when the stack is ready, 1 with what to do next on stderr otherwise.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
REPO="$PWD"

# shellcheck source=lib/common.sh
. "$REPO/scripts/lib/common.sh"

SRC="${REAL_API_SOURCE:-}"
# A ~ that reached here unexpanded, as from `env REAL_API_SOURCE=~/...` in zsh.
case "$SRC" in \~ | \~/*) SRC="$HOME${SRC#\~}" ;; esac
CLUSTER="${REAL_API_KIND_CLUSTER:-k0rdent-rc5}"
CTX="kind-$CLUSTER"
OFFSET="${REAL_API_PORT_OFFSET:-6000}"
ENV_FILE="${REAL_API_ENV_FILE:-$REPO/.live/real-api-kind.env}"
RUN_DIR="$REPO/.live/real-api-kind"
LOGS="$RUN_DIR/logs"

# What the API source's kind manifest seeds, and what this script seeds on top.
# The organization and the administrator are the manifest's; the rest are this
# stack's.
ORG=kind
ADMIN_ID=usr-kind-admin
# The ConfigMap that marks a kind cluster as one this script made.
MARKER=real-api-kind-setup
MACHINE_TYPE=mt-standard
POOL=pool-tenant-v4
# make live upgrades across this type's versions, so it gets three.
CLUSTER_TYPE=seed-ct-local
CLUSTER_TYPE_VERSIONS=3
# Subnets on a VPC and an interface on the worker pool, for full/'s subnets and
# security group. The walkthroughs take the newest type, and the API lists
# the newest first, so it's published after CLUSTER_TYPE.
SUBNET_TYPE=kind-ct-subnets
PROJECT_SA=sa-terraform-real-api-project
ORG_SA=sa-terraform-real-api-org
# What a kind stack can't do that a walkthrough asks for. TESTING.md says why.
GAPS="subnet-cidr no-file-store no-hosts"

port() { printf '%s' "$(( $1 + OFFSET ))"; }

# run_logged <name> <command...> runs a step with its output in $LOGS/<name>.log,
# and on a failure prints the log's end and fails with <next>.
run_logged() {
  local name="$1" next="$2"; shift 2
  local log="$LOGS/$name.log" started=$SECONDS
  note "$name: log in ${log#"$REPO"/}"
  if ! "$@" > "$log" 2>&1; then
    tail -20 "$log" >&2
    die "$name failed. $next"
  fi
  good "$name, $(( SECONDS - started ))s"
}

# make_src <target> [VAR=value...] runs a make target of the API source.
make_src() { make -C "$SRC" "$@"; }

# manifest_field <name> prints the administrator's <name> from the manifest.
manifest_field() {
  awk -v id="$ADMIN_ID" -v k="$1:" '$0 ~ "id: " id "$" {f=1} f && $1 == k {print $2; exit}' "$MANIFEST"
}

# src_var <name> prints a variable of the API source's Makefile.
src_var() {
  printf 'real-api-print:\n\t@printf "%%s\\n" "$(%s)"\n' "$1" \
    | make -s --no-print-directory -C "$SRC" -f Makefile -f - real-api-print
}

check_inputs() {
  local want want_commit have tool
  for tool in kind kubectl helm docker go jq curl python3 make git; do need "$tool"; done
  [ -n "$SRC" ] || die "REAL_API_SOURCE is not set. Check out the platform API's source at the version in ci/real-api/api-version and set REAL_API_SOURCE to it. TESTING.md has the steps."
  [ -f "$SRC/Makefile" ] && grep -q '^kind-install:' "$SRC/Makefile" \
    || die "$SRC has no kind-install make target. REAL_API_SOURCE must be the platform API's source checkout."
  case "$OFFSET" in ''|0[0-9]*|*[!0-9]*) die "REAL_API_PORT_OFFSET must be a whole number, such as 6000" ;; esac

  want="$(tr -d '[:space:]' < "$REPO/ci/real-api/api-version")"
  # The tag's commit, not a tag name read off HEAD, which could be another tag
  # on the same commit.
  want_commit="$(git -C "$SRC" rev-parse -q --verify "refs/tags/$want^{commit}")" \
    || die "$SRC has no tag $want, which ci/real-api/api-version names. Run: git -C $SRC fetch --tags && git -C $SRC checkout $want"
  have="$(git -C "$SRC" rev-parse HEAD)" || die "could not read $SRC's HEAD"
  [ "$have" = "$want_commit" ] || die "$SRC is at $(git -C "$SRC" rev-parse --short HEAD), not at $want, which ci/real-api/api-version names. Run: git -C $SRC checkout $want"
  [ -z "$(git -C "$SRC" status --porcelain --untracked-files=no)" ] \
    || die "$SRC has local changes. The stack has to run the API as tagged; commit or drop them first."
  VERSION="$want"

  # The kind manifest this script extends: the one YAML file in the source
  # that declares the administrator and its policies.
  MANIFEST="$(git -C "$SRC" grep -l --all-match -e '^users:$' -e '^policies:$' -e "^  - id: $ADMIN_ID\$" -- '*.yaml' '*.yml')"
  [ -n "$MANIFEST" ] && [ "$(printf '%s\n' "$MANIFEST" | wc -l | tr -d ' ')" = 1 ] \
    || die "$SRC has ${MANIFEST:+more than }one manifest declaring $ADMIN_ID with policies${MANIFEST:+, not one}. This script needs updating for the API's version."
  MANIFEST="$SRC/$MANIFEST"
  ADMIN_EMAIL="$(manifest_field email)"
  ADMIN_KIND="$(manifest_field principalType)"
  [ -n "$ADMIN_EMAIL" ] && [ -n "$ADMIN_KIND" ] || die "could not read $ADMIN_ID's email and principal type from ${MANIFEST#"$SRC"/}. This script needs updating for the API's version."
  CHART="$SRC/$(src_var HELM_UMBRELLA)"
  VALUES="$SRC/$(src_var KIND_VALUES)"
  [ -f "$CHART/Chart.yaml" ] && [ -f "$VALUES" ] \
    || die "the source's HELM_UMBRELLA and KIND_VALUES make variables don't name a chart and its kind values. This script needs updating for the API's version."

  private_dir "$REPO/.live"
  private_dir "$RUN_DIR"
  mkdir -p "$LOGS"
  good "API source at $VERSION, cluster $CLUSTER, gateway on localhost:$(port 8000)"
}

# write_overlay writes the values the stack needs on top of the chart's kind
# values: the API version's images, and the sign-in callback on this stack's
# forwarded auth port.
write_overlay() {
  OVERLAY="$RUN_DIR/values.yaml"
  cat > "$OVERLAY" <<EOF
global:
  imageTag: $VERSION
auth:
  env:
    AUTH_CALLBACK_URL: http://localhost:$(port 8083)/v1/regions/global/auth/callback
EOF
}

# write_manifest writes the kind manifest with more roles for the
# administrator: compute-admin on the organization, to issue the
# organization's service account the walkthroughs use, and the three platform
# roles that publish the region's catalog.
write_manifest() {
  local role scope
  STACK_MANIFEST="$RUN_DIR/manifest.yaml"
  grep -q '^policies:$' "$MANIFEST" || die "the source's kind manifest has no policies block. This script needs updating for the API's version."
  {
    sed -n '1,/^policies:$/p' "$MANIFEST"
    for role in compute-admin:org cluster-admin:platform compute-admin:platform infrastructure-admin:platform; do
      scope="${role#*:}"
      printf '  - role: %s\n    principal: {type: user, id: %s}\n' "${role%%:*}" "$ADMIN_ID"
      if [ "$scope" = org ]; then
        printf '    scope: {type: org, org: %s}\n' "$ORG"
      else
        printf '    scope: {type: platform}\n'
      fi
      printf '    displayName: %s on the %s, for the real-API check\n' "${role%%:*}" "$scope"
    done
    sed -n '/^policies:$/,$p' "$MANIFEST" | sed 1d
  } > "$STACK_MANIFEST"
}

# api_images prints the API's own images the chart deploys at this version,
# rendered from the chart, so a new service needs no change here.
api_images() {
  helm template real-api "$CHART" --set global.namespace=real-api \
      -f "$VALUES" -f "$OVERLAY" 2>/dev/null \
    | sed -n 's/^ *image: *"\{0,1\}\([^" ]*\)"\{0,1\}.*/\1/p' \
    | grep "^ghcr.io/.*:$VERSION\$" | sort -u
}

# load_images pulls each of the API's images on the host, where docker is
# logged in, and loads it into the node, which can't pull private images. An
# image the node already has is skipped.
load_images() {
  local platform img images node="$CLUSTER-control-plane"
  platform="linux/$(docker version --format '{{.Server.Arch}}')"
  images="$(api_images)"
  [ -n "$images" ] || { echo "the chart rendered no image tagged $VERSION"; return 1; }
  for img in $images; do
    if docker exec "$node" crictl inspecti "$img" >/dev/null 2>&1; then
      echo "on the node already: $img"
      continue
    fi
    docker image inspect "$img" >/dev/null 2>&1 || docker pull -q --platform "$platform" "$img" || {
      echo "could not pull $img"
      return 1
    }
    kind load docker-image "$img" --name "$CLUSTER" || return 1
  done
}

# claim_cluster refuses a kind cluster of that name that this script didn't
# make and that runs no platform API, unless REAL_API_KIND_REUSE=1, so the
# script never installs into a cluster that's another tool's. It sets REUSED
# to true for a cluster it didn't make. With REAL_API_FRESH=1 it deletes the
# cluster only when the marker says this script made it.
claim_cluster() {
  local made
  REUSED=false
  kind get clusters 2>/dev/null | grep -qx "$CLUSTER" || return 0
  made="$(kubectl --context "$CTX" -n default get configmap "$MARKER" -o jsonpath='{.data.made-by}' 2>/dev/null)"
  if [ -z "$made" ]; then
    REUSED=true
    if [ "${REAL_API_FRESH:-0}" = 1 ]; then
      die "REAL_API_FRESH=1 deletes only a kind cluster this script made, and $CLUSTER has no $MARKER ConfigMap in its default namespace. If nothing else needs it: kind delete cluster --name $CLUSTER, then run this again. Or set REAL_API_KIND_CLUSTER to another name."
    fi
    if [ -z "$(kubectl --context "$CTX" get deploy --all-namespaces --field-selector metadata.name=compute -o name 2>/dev/null)" ] \
        && [ "${REAL_API_KIND_REUSE:-0}" != 1 ]; then
      die "kind cluster $CLUSTER is there already, this script didn't make it, and it runs no platform API. Set REAL_API_KIND_CLUSTER to another name, or REAL_API_KIND_REUSE=1 to install into it anyway."
    fi
    return 0
  fi
  [ "$(kubectl --context "$CTX" -n default get configmap "$MARKER" -o jsonpath='{.data.reused}' 2>/dev/null)" != true ] \
    || REUSED=true
  [ "${REAL_API_FRESH:-0}" = 1 ] || return 0
  [ "$REUSED" = false ] \
    || die "REAL_API_FRESH=1 deletes only a kind cluster this script made, and $CLUSTER is one it found and installed into. If nothing else needs it: kind delete cluster --name $CLUSTER, then run this again."
  run_logged delete-cluster "Delete it by hand: kind delete cluster --name $CLUSTER" \
    kind delete cluster --name "$CLUSTER"
  REUSED=false
}

# mark_cluster leaves the ConfigMap claim_cluster looks for, saying whether
# this script made the cluster or installed into one it found.
mark_cluster() {
  kubectl --context "$CTX" -n default create configmap "$MARKER" \
      --from-literal=made-by=scripts/real-api-kind-setup.sh --from-literal=reused="$REUSED" \
      --dry-run=client -o yaml \
    | kubectl --context "$CTX" apply -f -
}

build_stack() {
  step "The cluster and the API"
  write_overlay
  write_manifest
  claim_cluster
  run_logged helm-deps "Check $SRC builds its charts: make -C $SRC helm-dep-build" \
    make_src helm-dep-build
  run_logged cluster "Check docker is running. To start again: REAL_API_FRESH=1 make real-api-stack" \
    make_src kind-cluster-up "KIND_CLUSTER_NAME=$CLUSTER"
  run_logged mark "Check kubectl reaches $CTX, then run this again." mark_cluster
  run_logged images "The images are private: docker login ghcr.io with an account that can read the API's packages, then run this again." \
    load_images
  run_logged upstream-images "Check docker can pull from Docker Hub, then run this again." \
    make_src kind-load-upstream "KIND_CLUSTER_NAME=$CLUSTER"
  run_logged install "Read the log, and kubectl --context $CTX get pods -A for a pod that isn't ready." \
    make_src kind-install "KIND_CLUSTER_NAME=$CLUSTER" MOCK_MODE=true "KIND_SECRETS=$OVERLAY"
  run_logged identity-provider "Read the log, then run this again." \
    make_src kind-multi-install-extras "KIND_GLOBAL_CLUSTER=$CLUSTER"
  run_logged provision "Read the log, then run this again. It's safe to repeat." \
    env "MANIFEST=$STACK_MANIFEST" make -C "$SRC" kind-multi-provision-mock "KIND_GLOBAL_CLUSTER=$CLUSTER"
}

# The forwarded ports.
set_urls() {
  GW="http://localhost:$(port 8000)"
  AUTH="http://localhost:$(port 8083)"
  MOCK="http://localhost:$(port 8190)"
}

http_code() { curl -sS -m 8 -o /dev/null -w '%{http_code}' "$1" 2>/dev/null || true; }

forward() {
  step "Port-forwards"
  run_logged port-forward "Read the log, then run this again." \
    make_src kind-port-forward "KIND_CLUSTER_NAME=$CLUSTER" "KIND_PF_PORT_OFFSET=$OFFSET"
  local gw auth _
  for _ in $(seq 1 30); do
    gw="$(http_code "$GW/")"
    auth="$(http_code "$AUTH/healthz")"
    case "$gw" in 000|''|5*) ;; *) [ "$auth" = 200 ] && break ;; esac
    sleep 2
  done
  case "$gw" in 000|''|5*) die "the gateway at $GW answered ${gw:-nothing}. $LOGS/port-forward.log says where each forward logs." ;; esac
  [ "$auth" = 200 ] || die "auth at $AUTH/healthz answered ${auth:-nothing}, not 200. $LOGS/port-forward.log says where each forward logs."
  good "the gateway answers on $GW"
}

# api <method> <path> [body] calls the gateway as the administrator, and
# prints the body, then the status on a last line.
api() {
  curl_bearer "$ADMIN" -sS -m 30 -w '\n%{http_code}' -X "$1" "$GW$2" \
    -H 'Content-Type: application/json' ${3:+--data "$3"}
}

# publish <what> <path> <body> creates one object, and takes one that's
# already there.
publish() {
  local resp code
  resp="$(api POST "$2" "$3")"
  code="${resp##*$'\n'}"
  case "$code" in
    201) good "published $1" ;;
    # 200 answers a repeat of the same body, 409 a different one.
    200|409) good "$1 is there already" ;;
    *) printf '%s\n' "${resp%$'\n'*}" | head -c 600 >&2; printf '\n' >&2
       die "publishing $1 answered ${code:-nothing}. Read $LOGS/provision.log for the administrator's roles, then run this again." ;;
  esac
}

available_versions() {
  local resp
  resp="$(api GET "/v1/regions/$REGION/compute/cluster-types/$1/versions?status=available")"
  [ "${resp##*$'\n'}" = 200 ] || return 1
  printf '%s' "${resp%$'\n'*}" | jq -r '[.versions[] | select(.status == "available")] | length'
}

# where sets REGION, the first region the API says holds projects, and
# PROJECT, the organization's project in it.
where() {
  local resp
  resp="$(api GET /v1/regions)"
  [ "${resp##*$'\n'}" = 200 ] || die "could not list the stack's regions (HTTP ${resp##*$'\n'})"
  REGION="$(printf '%s' "${resp%$'\n'*}" | jq -r '[.regions[] | select(.hostsProjects)][0].id // empty')"
  [ -n "$REGION" ] || die "the stack lists no region that holds projects"
  resp="$(api GET "/v1/regions/global/organizations/$ORG/projects")"
  [ "${resp##*$'\n'}" = 200 ] || die "could not list organization $ORG's projects (HTTP ${resp##*$'\n'})"
  PROJECT="$(printf '%s' "${resp%$'\n'*}" | jq -r --arg r "$REGION" '[.projects[] | select(.region == $r)][0].id // empty')"
  [ -n "$PROJECT" ] || die "organization $ORG has no project in region $REGION. Read $LOGS/provision.log."
}

cluster_type_body() {
  local subnets="$1"
  jq -nc --arg id "$2" --arg region "$REGION" --arg pool "$POOL" --arg mt "$MACHINE_TYPE" --argjson subnets "$subnets" '
    def net(id): {id: id, vpc: "fabric", type: "evpn", evpn: {type: "l2vpn"}};
    {
      id: $id,
      displayName: (if $subnets then "Kind cluster type with subnets" else "Kind cluster type" end),
      networkSchema: (if $subnets then {
        vpcs: [{id: "fabric", backend: "nico"}],
        networks: [net("mgmt"), net("data")],
        subnets: [{id: "mgmt", network: "mgmt", cidr: "10.0.200.0/24"},
                  {id: "data", network: "data", prefixLength: 26,
                   addressPool: ("/v1/regions/" + $region + "/networking/address-pools/" + $pool)}]
      } else {
        vpcs: [{id: "fabric", backend: "nico"}],
        networks: [net("mgmt")]
      } end),
      k8s: {k0sNetworkProvider: "calico", k0sVersion: "v1.36.1+k0s.0",
            podCidrs: ["10.0.0.0/17"], serviceCidrs: ["10.96.0.0/12"],
            clusterTemplateName: "tmpl-v1", clusterTemplateVersion: "v1", services: [],
            controlPlane: {type: "hcp", replicas: 3, bootstrapConfig: {}, opConfig: {}},
            workers: {defaults: {}}},
      nodePools: [{id: "worker", role: "worker", machineType: $mt, nodeCountDefault: 2,
                   networkAttachment: (if $subnets then {ethernets: {eth0: {connectToNetwork: {name: "mgmt"}}}} else {} end)}]
    }'
}

seed() {
  step "What the check builds from"
  local have code tries=0
  ADMIN="$(admin_token)" || exit 1
  good "signed in as $ADMIN_EMAIL through the mock identity provider"
  where
  good "region $REGION, project $PROJECT"

  publish "machine type $MACHINE_TYPE" "/v1/regions/$REGION/infrastructure/machine-types" \
    "$(jq -nc --arg n "$MACHINE_TYPE" '{name: $n, infrastructureProvider: "nico", selector: {},
         config: {image: {infrastructureProvider: "nico", url: "https://example.invalid/image.qcow2",
                          checksum: "0000000000000000000000000000000000000000000000000000000000000000"}}}')"
  publish "address pool $POOL" "/v1/regions/$REGION/networking/address-pools" \
    "$(jq -nc --arg id "$POOL" '{id: $id, displayName: "Tenant subnets (kind)", routable: false,
         ipVersion: "IPV4", prefixes: ["10.0.128.0/18"], minPrefixLength: 20, maxPrefixLength: 30}')"
  publish "cluster type $CLUSTER_TYPE" "/v1/regions/$REGION/compute/cluster-types" \
    "$(cluster_type_body false "$CLUSTER_TYPE")"

  # A new version carries every field of the one before, so only the
  # description changes. In mock mode an upgrade between them runs the API's
  # whole upgrade path but the build.
  have="$(available_versions "$CLUSTER_TYPE")" || die "could not list the versions of $CLUSTER_TYPE"
  while [ "$have" -lt "$CLUSTER_TYPE_VERSIONS" ]; do
    tries=$((tries + 1))
    [ "$tries" -le "$CLUSTER_TYPE_VERSIONS" ] || die "published versions of $CLUSTER_TYPE, but only $have are available"
    code="$(api POST "/v1/regions/$REGION/compute/cluster-types/$CLUSTER_TYPE/versions" \
      "$(jq -nc --arg d "Version $((have + 1))" '{status: "available", description: $d}')" | tail -1)"
    case "$code" in 200|201) ;; *) die "publishing a version of $CLUSTER_TYPE answered ${code:-nothing}, not 201" ;; esac
    have="$(available_versions "$CLUSTER_TYPE")" || die "could not list the versions of $CLUSTER_TYPE"
  done
  good "$CLUSTER_TYPE has $have available versions"

  publish "cluster type $SUBNET_TYPE" "/v1/regions/$REGION/compute/cluster-types" \
    "$(cluster_type_body true "$SUBNET_TYPE")"
}

# mint <client id> <client secret> prints an access token, or nothing.
mint() {
  token_request_body "$1" "$2" "" \
    | curl -sS -m 15 -X POST "$GW/v1/regions/global/auth/token" \
        -H 'Content-Type: application/json' --data-binary @- 2>/dev/null \
    | jq -r '.access_token // empty' 2>/dev/null
}

# credentials_work <project id> <project secret> <org id> <org secret>
# succeeds when both credentials mint, and the project's can read the
# project's clusters.
credentials_work() {
  local tok
  [ -n "$1" ] && [ -n "$2" ] && [ -n "$3" ] && [ -n "$4" ] || return 1
  tok="$(mint "$1" "$2")"
  [ -n "$tok" ] || return 1
  [ "$(curl_bearer "$tok" -sS -m 15 -o /dev/null -w '%{http_code}' \
        "$GW/v1/regions/$REGION/projects/$PROJECT/compute/clusters")" = 200 ] || return 1
  [ -n "$(mint "$3" "$4")" ]
}

# env_file_credentials prints the env file's four credential values, one a
# line, or nothing when there's no env file. It sources the file in a
# subshell, so nothing else in it reaches this script.
env_file_credentials() {
  [ -f "$ENV_FILE" ] || return 0
  (
    # shellcheck source=/dev/null # written by write_env_file
    . "$ENV_FILE" >/dev/null 2>&1 || exit 0
    id_var="${ENV_PREFIX}CLIENT_ID" secret_var="${ENV_PREFIX}CLIENT_SECRET"
    printf '%s\n' "${!id_var:-}" "${!secret_var:-}" "${REAL_API_ORG_CLIENT_ID:-}" "${REAL_API_ORG_CLIENT_SECRET:-}"
  )
}

# signin_client prints the sign-in client the installed chart's values name
# for the administrator's principal type.
signin_client() {
  helm --kube-context "$CTX" -n "$(src_var KIND_NAMESPACE)" get values "$(src_var KIND_RELEASE_NAME)" -a -o json 2>/dev/null \
    | jq -r --arg k "$ADMIN_KIND" '[.. | objects | .principalAudiences? | objects | .[$k] // empty][0] // empty'
}

# admin_token signs in as the manifest's administrator through the mock
# identity provider, once a run, and prints an access token.
admin_token() {
  local resp code authz tok client
  client="$(signin_client)"
  [ -n "$client" ] || die "could not read the sign-in client for $ADMIN_KIND users from the installed chart's values. This script needs updating for the API's version."
  resp="$(jq -nc --arg e "$ADMIN_EMAIL" --arg c "$client" \
            '{email: $e, redirectUri: "http://localhost:8083/auth/complete", clientId: $c}' \
    | curl -sS -m 15 -w '\n%{http_code}' -H 'Content-Type: application/json' -X POST \
        "$AUTH/v1/regions/global/auth/login/initiate" --data-binary @- 2>/dev/null)"
  code="${resp##*$'\n'}"
  case "$code" in
    200) ;;
    429) die "the sign-in for $ADMIN_EMAIL answered 429. Wait a minute, then run this again." ;;
    *) die "could not start a sign-in for $ADMIN_EMAIL at $AUTH (HTTP ${code:-nothing}). Read $LOGS/provision.log: the provisioning seeds the user." ;;
  esac
  authz="$(printf '%s' "${resp%$'\n'*}" | jq -r '.authorizationUrl // empty' | sed "s|^https\{0,1\}://[^/]*|$MOCK|")"
  [ -n "$authz" ] || die "the sign-in answer carried no authorizationUrl"
  tok="$(curl -sS -m 20 -L --max-redirs 5 -v -o /dev/null "$authz" 2>&1 \
    | sed -n 's/^< [Ll]ocation: //p' | tr -d '\r' | tr '&#?' '\n\n\n' \
    | sed -n 's/^access_token=//p' | head -1)"
  [ -n "$tok" ] || die "the mock identity provider gave no token for $ADMIN_EMAIL. Check the forward to $MOCK."
  printf '%s' "$tok"
}

# service_account <admin token> <id> <scope> <scope id> <roles JSON> creates
# the service account, or takes the one there, and issues it a credential. It
# sets SA_CLIENT_ID and SA_CLIENT_SECRET.
service_account() {
  local tok="$1" id="$2" base resp code
  base="$GW/v1/regions/global/organizations/$ORG/iam/service-accounts"
  resp="$(curl_bearer "$tok" -sS -m 20 -w '\n%{http_code}' -X POST "$base" -H 'Content-Type: application/json' \
    --data "$(jq -nc --arg id "$id" --arg s "$3" --arg i "$4" --argjson r "$5" \
               '{id: $id, displayName: $id, scope: $s, scopeId: $i, roles: $r}')")"
  code="${resp##*$'\n'}"
  case "$code" in
    200|201) ;;
    409) resp="$(curl_bearer "$tok" -sS -m 20 -w '\n%{http_code}' "$base/$id")"
         [ "${resp##*$'\n'}" = 200 ] || die "could not read service account $id (HTTP ${resp##*$'\n'})" ;;
    *) printf '%s\n' "${resp%$'\n'*}" | head -c 600 >&2; printf '\n' >&2
       die "creating service account $id answered ${code:-nothing}" ;;
  esac
  SA_CLIENT_ID="$(printf '%s' "${resp%$'\n'*}" | jq -r '.clientId // empty')"
  [ -n "$SA_CLIENT_ID" ] || die "service account $id has no clientId"

  resp="$(curl_bearer "$tok" -sS -m 20 -w '\n%{http_code}' -X POST "$base/$id/credentials" \
    -H 'Content-Type: application/json' --data '{}')"
  code="${resp##*$'\n'}"
  case "$code" in
    200|201) ;;
    *) printf '%s\n' "${resp%$'\n'*}" | jq -c 'del(.clientSecret)' 2>/dev/null | head -c 600 >&2; printf '\n' >&2
       die "issuing a credential for $id answered ${code:-nothing}. A service account holds five at most; to start clean: REAL_API_FRESH=1 make real-api-stack" ;;
  esac
  SA_CLIENT_SECRET="$(printf '%s' "${resp%$'\n'*}" | jq -r '.clientSecret // empty')"
  [ -n "$SA_CLIENT_SECRET" ] || die "the credential for $id came back with no secret"
  good "service account $id, roles $5"
}

# env_line <name> <value> prints an export line that sources back to <value>
# whatever it holds: single quotes, each ' in it closed, written as "'" and
# reopened. Not printf %q: bash 3.2 leaves a leading ~ bare.
env_line() {
  local sq="'" q="'\"'\"'" v="$2"
  printf "export %s='%s'\n" "$1" "${v//$sq/$q}"
}

credentials() {
  step "Service accounts"
  local pcid psec ocid osec
  { IFS= read -r pcid; IFS= read -r psec; IFS= read -r ocid; IFS= read -r osec; } < <(env_file_credentials)
  if credentials_work "$pcid" "$psec" "$ocid" "$osec"; then
    good "the credentials in $ENV_FILE still work"
  else
    service_account "$ADMIN" "$PROJECT_SA" project "$PROJECT" '["compute-admin","credentials-admin"]'
    pcid="$SA_CLIENT_ID" psec="$SA_CLIENT_SECRET"
    service_account "$ADMIN" "$ORG_SA" organization "$ORG" '["compute-admin","organizations-admin","iam-viewer"]'
    ocid="$SA_CLIENT_ID" osec="$SA_CLIENT_SECRET"
    credentials_work "$pcid" "$psec" "$ocid" "$osec" \
      || die "the new credentials don't work. Run this again; if it fails the same way, start clean: REAL_API_FRESH=1 make real-api-stack"
  fi
  write_env_file "$pcid" "$psec" "$ocid" "$osec"
}

# write_env_file <project id> <project secret> <org id> <org secret> writes the
# env file whole, every run, so a changed setting never outlives the run that
# changed it. Only the credentials carry over. A directory it makes is
# private; one that's already there is left as it is.
write_env_file() {
  local dir
  dir="$(dirname "$ENV_FILE")"
  [ -d "$dir" ] || private_dir "$dir"
  ( umask 077
    {
      printf '# Written by scripts/real-api-kind-setup.sh for make real-api-check. Holds secrets.\n'
      env_line "${ENV_PREFIX}ENDPOINT" "$GW"
      env_line "${ENV_PREFIX}REGION" "$REGION"
      env_line "${ENV_PREFIX}PROJECT" "$PROJECT"
      env_line "${ENV_PREFIX}CLIENT_ID" "$1"
      env_line "${ENV_PREFIX}CLIENT_SECRET" "$2"
      env_line REAL_API_ORG_CLIENT_ID "$3"
      env_line REAL_API_ORG_CLIENT_SECRET "$4"
      env_line REAL_API_CLUSTER_TYPE "$CLUSTER_TYPE"
      env_line REAL_API_WALKTHROUGH_GAPS "$GAPS"
      env_line REAL_API_KUBE_CONTEXT "$CTX"
      printf 'unset %sAPI_KEY\n' "$ENV_PREFIX"
    } > "$ENV_FILE.new" && mv "$ENV_FILE.new" "$ENV_FILE" ) || die "could not write $ENV_FILE"
  chmod 600 "$ENV_FILE" || die "could not make $ENV_FILE private"
  good "wrote $ENV_FILE, mode 600"
}

main() {
  local started=$SECONDS
  check_inputs
  read_names
  set_urls
  build_stack
  forward
  seed
  credentials
  printf '\n%sReady%s in %ss. API %s on %s, gateway %s.\n\n' "$GREEN$B" "$R" "$(( SECONDS - started ))" "$VERSION" "$CTX" "$GW"
  printf 'Run the check from this repository:\n\n'
  printf '    . %s\n    make real-api-check\n\n' "$ENV_FILE"
}

[ $# -eq 0 ] || die "usage: REAL_API_SOURCE=<checkout> scripts/real-api-kind-setup.sh. It takes no arguments; the header lists the variables it reads."
main
