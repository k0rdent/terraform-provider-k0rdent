#!/usr/bin/env bash
#
# Setup preflight. Answers one question, "can this machine run the tests?",
# before anything long and confusing has a chance to fail halfway.
#
#   scripts/doctor.sh          check tier 1, and report tier 2 separately
#
# Tier 1 is the local fake: `make check`, `make demo`, `make plan-fake`.
# Everything it needs is in the REQUIRED list below and a
# missing one is an error.
#
# Tier 2 is a local kind stack serving the real API on http://localhost:8000.
# Its tools are reported separately and a missing one is a warning, because
# most people run tier 1 first and never need them.
#
# Exit status is 0 when tier 1 can run, 1 when it cannot. Tier 2 never fails
# this script.

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
REPO="$PWD"

if [ -t 1 ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; RED=$'\033[31m'; R=$'\033[0m'
else
  B=''; DIM=''; GREEN=''; YELLOW=''; RED=''; R=''
fi

step() { printf '\n%s━━ %s %s\n' "$B" "$1" "$R"; }
good() { printf '%s   ok   %s%s\n' "$GREEN" "$1" "$R"; }
warn() { printf '%s   warn %s%s\n' "$YELLOW" "$1" "$R"; }
bad()  { printf '%s   MISS %s%s\n' "$RED" "$1" "$R"; }
note() { printf '%s        %s%s\n' "$DIM" "$1" "$R"; }

os_kind() {
  case "$(uname -s)" in
    Darwin) echo darwin ;;
    Linux) echo linux ;;
    *) echo other ;;
  esac
}

OS="$(os_kind)"

# Compare dotted versions on major.minor.patch. A missing part counts as 0.
# Strips a leading "v" or "go". Returns 0 when $1 >= $2.
semver_ge() {
  local left="${1#v}"
  local right="${2#v}"
  left="${left#go}"
  right="${right#go}"
  local l r i
  local -a lp rp
  IFS=. read -r -a lp <<< "$left"
  IFS=. read -r -a rp <<< "$right"
  for i in 0 1 2; do
    l="${lp[$i]:-0}"
    r="${rp[$i]:-0}"
    l="${l%%[!0-9]*}"
    r="${r%%[!0-9]*}"
    l="${l:-0}"
    r="${r:-0}"
    [ "$l" -gt "$r" ] 2>/dev/null && return 0
    [ "$l" -lt "$r" ] 2>/dev/null && return 1
  done
  return 0
}

hint_make() {
  case "$OS" in
    darwin) note "install: xcode-select --install   or: brew install make" ;;
    linux) note "install: sudo apt-get install -y make" ;;
    *) note "install GNU make" ;;
  esac
}

hint_go() {
  case "$OS" in
    darwin) note "install: brew install go" ;;
    linux) note "install: https://go.dev/doc/install  (need ${GO_MIN}+; apt's golang-go is often too old)" ;;
    *) note "install Go ${GO_MIN}+ from https://go.dev/doc/install" ;;
  esac
}

hint_terraform() {
  case "$OS" in
    darwin) note "install: brew install hashicorp/tap/terraform   or: brew install opentofu" ;;
    linux) note "install: HashiCorp apt, https://developer.hashicorp.com/terraform/install  or OpenTofu" ;;
    *) note "install Terraform from https://developer.hashicorp.com/terraform/install" ;;
  esac
}

hint_python() {
  case "$OS" in
    darwin) note "install: brew install python" ;;
    linux) note "install: sudo apt-get install -y python3" ;;
    *) note "install Python 3" ;;
  esac
}

hint_golangci() {
  note "install: make lint-install"
  note "that pins golangci-lint v2 and builds it with this module's Go version"
  note 'then: export PATH="$(go env GOPATH)/bin:$PATH"  if the binary is not already on PATH'
}

hint_tier2() {
  local cmd="$1"
  case "$cmd" in
    docker)
      case "$OS" in
        darwin) note "install Docker Desktop or OrbStack" ;;
        linux) note "install Docker Engine: https://docs.docker.com/engine/install/" ;;
        *) note "install Docker" ;;
      esac
      ;;
    kind)
      case "$OS" in
        darwin) note "install: brew install kind" ;;
        linux) note "install kind: https://kind.sigs.k8s.io/docs/user/quick-start/#installation" ;;
        *) note "install kind: https://kind.sigs.k8s.io/docs/user/quick-start/#installation" ;;
      esac
      ;;
    helm)
      case "$OS" in
        darwin) note "install: brew install helm" ;;
        linux) note "install helm: https://helm.sh/docs/intro/install/" ;;
        *) note "install helm: https://helm.sh/docs/intro/install/" ;;
      esac
      ;;
    kubectl)
      case "$OS" in
        darwin) note "install: brew install kubectl" ;;
        linux) note "install kubectl: https://kubernetes.io/docs/tasks/tools/" ;;
        *) note "install kubectl: https://kubernetes.io/docs/tasks/tools/" ;;
      esac
      ;;
    jq)
      case "$OS" in
        darwin) note "install: brew install jq" ;;
        linux) note "install: sudo apt-get install -y jq" ;;
        *) note "install jq" ;;
      esac
      ;;
    uv)
      case "$OS" in
        darwin) note "install: brew install uv" ;;
        linux) note "install uv: curl -LsSf https://astral.sh/uv/install.sh | sh" ;;
        *) note "install uv: https://docs.astral.sh/uv/getting-started/installation/" ;;
      esac
      ;;
    *)
      note "install $cmd"
      ;;
  esac
}

tier1_missing=0
tier2_missing=0
tier2_missing_cmds=""

# have <command> <label> <install hint>
have() {
  if command -v "$1" >/dev/null 2>&1; then
    good "$2"
    return 0
  fi
  return 1
}

printf '%sk0rdent AI Terraform provider: setup check%s\n' "$B" "$R"
note "repository: $REPO"

# ---------------------------------------------------------------------------
step "Tier 1: the local fake. Everything here is required."
# ---------------------------------------------------------------------------

# make: every command this suggests is a make target. Doctor is usually
# invoked as `make doctor`, but running this script directly still needs make
# for the five tests that follow.
if have make "$(make --version 2>/dev/null | head -1)"; then
  :
else
  bad "make is not on PATH"
  note "every tier-1 command is a make target; doctor itself is usually 'make doctor'"
  hint_make
  tier1_missing=$((tier1_missing + 1))
fi

# Go, with a version floor: the full version go.mod declares. 1.26.10 is the
# first 1.26 with the crypto/x509 fix, and golang.org/x/net v0.60.0 needs at
# least 1.26.0. Go refuses to build a module that declares a newer version than
# the toolchain, so a lower one fails outright rather than degrading.
MODULE_GO="$(awk '/^go / {print $2; exit}' go.mod 2>/dev/null || true)"
GO_MIN="${MODULE_GO:-1.26.10}"
if command -v go >/dev/null 2>&1; then
  go_ver="$(go env GOVERSION 2>/dev/null | sed 's/^go//')"
  if semver_ge "${go_ver:-0}" "$GO_MIN"; then
    good "go $go_ver"
  else
    bad "go $go_ver is too old; need $GO_MIN or newer"
    note "go.mod declares go $GO_MIN, and a lower toolchain cannot build it"
    hint_go
    tier1_missing=$((tier1_missing + 1))
  fi
else
  bad "go is not on PATH"
  hint_go
  tier1_missing=$((tier1_missing + 1))
fi

# Terraform or OpenTofu. Either is fine; the scripts prefer terraform.
if command -v terraform >/dev/null 2>&1; then
  good "terraform $(terraform version 2>/dev/null | head -1 | awk '{print $2}')"
elif command -v tofu >/dev/null 2>&1; then
  good "tofu $(tofu version 2>/dev/null | head -1 | awk '{print $2}') (terraform not found; scripts fall back to this)"
else
  bad "neither terraform nor tofu is on PATH"
  hint_terraform
  tier1_missing=$((tier1_missing + 1))
fi

# golangci-lint: `make check` runs it. v1 cannot load `.golangci.yml` (version:
# "2"). A v2 binary built with a Go older than the module fails with:
#   the Go language version (go1.XX) used to build golangci-lint is lower
#   than the targeted Go version (...)
# Unversioned `@latest` can also demand a newer Go than go.mod. Always install
# via `make lint-install`.
if command -v golangci-lint >/dev/null 2>&1; then
  lint_out="$(golangci-lint version 2>/dev/null || true)"
  lint_ver="$(printf '%s\n' "$lint_out" | sed -n 's/.* version v*\([0-9][0-9.]*\).*/\1/p' | head -1)"
  lint_go="$(printf '%s\n' "$lint_out" | sed -n 's/.*built with go\([0-9][0-9.]*\).*/\1/p' | head -1)"
  if [ -z "$lint_ver" ]; then
    bad "golangci-lint is on PATH but its version could not be parsed"
    note "output was: ${lint_out:-<empty>}"
    hint_golangci
    tier1_missing=$((tier1_missing + 1))
  elif ! semver_ge "$lint_ver" "2"; then
    bad "golangci-lint $lint_ver is too old; need v2 (this repo's .golangci.yml is version 2)"
    hint_golangci
    tier1_missing=$((tier1_missing + 1))
  elif [ -n "$lint_go" ] && ! semver_ge "$lint_go" "$GO_MIN"; then
    bad "golangci-lint $lint_ver was built with go$lint_go; need go${GO_MIN}+ to lint this module"
    note "make check fails: the Go language version used to build golangci-lint is lower than the targeted Go version"
    hint_golangci
    tier1_missing=$((tier1_missing + 1))
  else
    if [ -n "$lint_go" ]; then
      good "golangci-lint $lint_ver (built with go$lint_go)"
    else
      good "golangci-lint $lint_ver"
      warn "could not read the Go version golangci-lint was built with"
      note "if make check fails on the linter, run: make lint-install"
    fi
  fi
else
  bad "golangci-lint is not on PATH"
  note "make check runs it"
  hint_golangci
  tier1_missing=$((tier1_missing + 1))
fi

# python3: ci/plan-against-fake.sh reads JSON plans with it.
if have python3 "python3 $(python3 --version 2>&1 | awk '{print $2}')"; then
  :
else
  bad "python3 is not on PATH"
  note "make plan-fake reads Terraform's JSON plan output with it"
  hint_python
  tier1_missing=$((tier1_missing + 1))
fi

# A dev override that has gone missing, which Terraform will not stop for.
#
# `make demo` and `make live` each write their own
# terraform.rc into a run directory, so none of them can hit this. Driving
# examples/ by hand can: docs/walkthrough.md asks you to write the file
# yourself, under your cache directory (under /tmp, which macOS purges, before
# that). Come back after it has been cleared with
# TF_CLI_CONFIG_FILE still exported from your shell profile and the file gone,
# and Terraform prints "Unable to open CLI configuration file" as a WARNING,
# carries on without the override, tries to resolve the provider from a
# registry it was never published to, and fails several steps later with
# "Inconsistent dependency lock file". Nothing in that message names the cause.
if [ -n "${TF_CLI_CONFIG_FILE:-}" ]; then
  if [ ! -r "$TF_CLI_CONFIG_FILE" ]; then
    bad "TF_CLI_CONFIG_FILE points at a file that is not there"
    note "    $TF_CLI_CONFIG_FILE"
    note ""
    note "Terraform treats this as a warning, ignores your dev override, and fails"
    note "later with a lock-file error that does not mention it. Re-run step 1 of"
    note "docs/walkthrough.md, or unset the variable."
    tier1_missing=$((tier1_missing + 1))
  else
    # The first "<source address>" = "<directory>" pair inside dev_overrides,
    # whether the block is written on one line or across several.
    override_pair=$(awk '
      /dev_overrides[[:space:]]*\{/ { inblock = 1; sub(/.*dev_overrides[[:space:]]*\{/, "") }
      inblock {
        if (match($0, /"[^"]*"[[:space:]]*=[[:space:]]*"[^"]*"/)) {
          pair = substr($0, RSTART, RLENGTH)
          n = split(pair, part, "\"")
          print part[2] "\t" part[4]
          exit
        }
        if ($0 ~ /\}/) exit
      }' "$TF_CLI_CONFIG_FILE")
    override_addr=${override_pair%%$'\t'*}
    override_dir=${override_pair#*$'\t'}
    # Terraform looks for terraform-provider-<type>, the type being the last
    # segment of the source address, with or without a registry host.
    override_type=${override_addr##*/}
    if [ -z "$override_pair" ]; then
      bad "TF_CLI_CONFIG_FILE has no dev override in it"
      note "    $TF_CLI_CONFIG_FILE"
      note ""
      note "Terraform will resolve the provider from a registry it was never published"
      note "to, and fail with a lock-file error. Re-run step 1 of docs/walkthrough.md, or"
      note "a make live-preflight for docs/walkthrough-hardware.md."
      tier1_missing=$((tier1_missing + 1))
    elif [ ! -x "$override_dir/terraform-provider-${override_type:-k0rdent}" ]; then
      bad "the dev override names a directory with no provider binary in it"
      note "    $override_dir"
      note ""
      note "Terraform will fall back to the registry and fail with a lock-file error."
      note "Rebuild into it: go build -o \"$override_dir/\" ."
      tier1_missing=$((tier1_missing + 1))
    else
      good "dev override resolves to a provider binary in $override_dir"
    fi
  fi
fi

# ---------------------------------------------------------------------------
step "Tier 2: a local kind stack. Only needed for 'make live'."
# ---------------------------------------------------------------------------
note "Skip this section if you are only running tier 1."

for pair in \
  "docker:Docker (OrbStack, Docker Desktop, or Docker Engine)" \
  "kind:kind" \
  "helm:helm" \
  "kubectl:kubectl" \
  "jq:jq" \
  "uv:uv"
do
  cmd="${pair%%:*}"
  label="${pair#*:}"
  if command -v "$cmd" >/dev/null 2>&1; then
    good "$label"
  else
    warn "$label is not on PATH"
    hint_tier2 "$cmd"
    tier2_missing=$((tier2_missing + 1))
    # Docker is a desktop/engine install, not a brew/apt formula in the same way.
    [ "$cmd" = "docker" ] || tier2_missing_cmds="${tier2_missing_cmds:+$tier2_missing_cmds }$cmd"
  fi
done

# Docker being installed is not the same as Docker running, and the kind stack
# needs real CPU. The default OrbStack allocation is 2, which is not enough:
# pods sit Pending on "Insufficient cpu" and the helm install times out.
KIND_MIN_CPU=6
if command -v docker >/dev/null 2>&1; then
  if docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
    good "docker daemon is running"
    cpus="$(docker info --format '{{.NCPU}}' 2>/dev/null)"
    case "$cpus" in
      ''|*[!0-9]*) warn "could not read the docker CPU count" ;;
      *)
        if [ "$cpus" -ge "$KIND_MIN_CPU" ]; then
          good "docker has $cpus CPUs"
        else
          warn "docker has only $cpus CPUs; the kind stack needs about $KIND_MIN_CPU"
          note "with fewer, pods stay Pending on 'Insufficient cpu' and 'make kind-up' times out"
          case "$OS" in
            darwin)
              note "OrbStack:        orb config set cpu 8 && orb config set memory_mib 16384 && orb stop && orb start"
              note "Docker Desktop:  Settings > Resources > CPUs"
              ;;
            linux)
              note "give the Docker Engine VM or host at least ${KIND_MIN_CPU} CPUs"
              ;;
            *)
              note "raise the Docker CPU allocation to about ${KIND_MIN_CPU}"
              ;;
          esac
          tier2_missing=$((tier2_missing + 1))
        fi
        ;;
    esac
  else
    warn "docker is installed but the daemon is not running"
    case "$OS" in
      darwin) note "start OrbStack or Docker Desktop, then re-run this" ;;
      linux) note "start the daemon: sudo systemctl start docker   (or equivalent), then re-run this" ;;
      *) note "start the Docker daemon, then re-run this" ;;
    esac
    tier2_missing=$((tier2_missing + 1))
  fi
fi

if [ -n "$tier2_missing_cmds" ]; then
  note ""
  case "$OS" in
    darwin)
      note "install the missing ones with: brew install $tier2_missing_cmds"
      ;;
    linux)
      note "missing tier-2 tools: $tier2_missing_cmds. Linux hints are next to each warn above (not Homebrew)"
      ;;
    *)
      note "missing tier-2 tools: $tier2_missing_cmds"
      ;;
  esac
fi

# ---------------------------------------------------------------------------
step "Result"
# ---------------------------------------------------------------------------

if [ "$tier1_missing" -gt 0 ]; then
  printf '%s%sTier 1 cannot run%s: %d prerequisite(s) missing above.\n\n' \
    "$RED" "$B" "$R" "$tier1_missing"
  exit 1
fi

printf '%s%sTier 1 is ready.%s Run, in order:\n\n' "$GREEN" "$B" "$R"
printf '    cd %s\n' "$REPO"
printf '    make check\n'
printf '    make demo\n'
printf '    make plan-fake\n\n'

if [ "$tier2_missing" -gt 0 ]; then
  printf '%sTier 2 is not ready%s: %d item(s) flagged above. That is fine unless you\n' \
    "$YELLOW" "$R" "$tier2_missing"
  printf 'are running %smake live%s. See TESTING.md.\n\n' "$B" "$R"
else
  printf '%sTier 2 looks ready too.%s See TESTING.md for the kind bring-up.\n\n' "$GREEN" "$R"
fi
