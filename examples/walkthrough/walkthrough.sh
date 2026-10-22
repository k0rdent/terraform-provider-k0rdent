#!/usr/bin/env bash
#
# The quick path through docs/walkthrough-hardware.md. It runs the same
# commands the page shows, and prints each one first, so you can follow along
# or do it by hand next time.
#
# Usage:
#
#   examples/walkthrough/walkthrough.sh setup [ref]   from a clone
#   ./walkthrough.sh up <directory>     plan, apply, plan again
#   ./walkthrough.sh check <directory>  plan only
#   ./walkthrough.sh down <directory>   destroy
#
# setup starts from a clean slate every time. It makes a new run folder under
# ~/k0rdent-runs (or WALKTHROUGH_RUNS) with a worktree of exactly <ref>,
# origin/main unless you name one, a provider built from it, a terraform.rc
# pointing at that build, and a copy of that ref's walkthrough. Each
# terraform.tfvars comes from ~/.config/k0rdent-walkthrough/<directory>.tfvars
# when you keep one there, and ships empty otherwise.
#
# up, check and down run in the run folder that holds this script, so the
# state stays there and a destroy uses the build that made it. They need
# `. ./setup.sh` in this shell first. Arguments after the directory go to
# `terraform apply` or `terraform destroy`.
#
# Status: 0 when it worked, 1 with the next step on stderr otherwise. up exits
# 1 when the plan after the apply proposes a change.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
runs="${WALKTHROUGH_RUNS:-$HOME/k0rdent-runs}"
config="${XDG_CONFIG_HOME:-$HOME/.config}/k0rdent-walkthrough"
tf="${TF_BIN:-terraform}"

# say prints a command as the page writes it, with ~ for your home directory.
say() {
  local line="$*" tilde='~'
  printf '\n$ %s\n' "${line//"$HOME"/$tilde}" >&2
}
run() { say "$*"; "$@"; }
stop() { printf '\n%s\n' "$*" >&2; exit 1; }

setup() {
  local ref="${1:-origin/main}" repo run_dir sha f d
  repo="$(git -C "$here" rev-parse --show-toplevel 2>/dev/null)" ||
    stop "setup works from a clone. Run it as examples/walkthrough/walkthrough.sh setup there."

  say "cd $repo"
  cd "$repo"
  if [[ "$ref" == origin/* ]]; then
    run git fetch origin || stop "git fetch failed, so $ref may be stale. Fix access, or name a local ref."
  fi
  sha="$(git rev-parse --verify --quiet "$ref^{commit}")" || stop "No commit $ref here."

  run_dir="$runs/$(date -u +%Y-%m-%d-%H%M%S)-$(printf '%s' "$ref" | tr -c 'A-Za-z0-9.\n' '-')"
  [[ ! -e "$run_dir" ]] || stop "$run_dir already exists. Run setup again in a second."
  run mkdir -p "$run_dir/bin"
  run chmod 700 "$run_dir"
  run git worktree add --detach "$run_dir/provider" "$sha"

  say "cd $run_dir/provider"
  cd "$run_dir/provider"
  run go build -o "$run_dir/bin/terraform-provider-k0rdent" . ||
    stop "The build failed. docs/build-from-source.md has what to check."
  run cp -R examples/walkthrough "$run_dir/walkthrough"

  printf '\nWriting %s, which points Terraform at the build in bin/.\n' "$run_dir/terraform.rc" >&2
  printf '%s\n' 'provider_installation {' \
    "  dev_overrides { \"k0rdent/k0rdent\" = \"$run_dir/bin\" }" \
    '  direct {}' '}' > "$run_dir/terraform.rc"

  for f in "$run_dir"/walkthrough/*/terraform.tfvars; do
    d="$(basename "$(dirname "$f")")"
    if [[ -f "$config/$d.tfvars" ]]; then
      run cp "$config/$d.tfvars" "$f"
    fi
  done

  printf '\nBuilt %s from %s. Next:\n\n' "$(git log -1 --format='%h %s')" "$ref" >&2
  printf '  cd %s/walkthrough\n  . ./setup.sh\n  ./walkthrough.sh up look\n' "$run_dir" >&2
}

# ready <directory>: a run folder, its build in this shell, a credential, and
# a filled-in terraform.tfvars.
ready() {
  local rc
  rc="$(cd "$here/.." && pwd)/terraform.rc"
  [[ -f "$rc" ]] ||
    stop "This isn't a run folder. Make one with walkthrough.sh setup from a clone."
  [[ -d "$here/$1" ]] || stop "No directory $1 in $here."
  [[ "${TF_CLI_CONFIG_FILE:-}" == "$rc" && -n "${K0RDENT_ENDPOINT:-}" ]] ||
    stop "This shell isn't set up for this run folder. Run . ./setup.sh in $here first."
  [[ -n "${K0RDENT_CLIENT_SECRET:-}${K0RDENT_API_KEY:-}" ]] ||
    stop "No credential in this shell. Run . ./setup.sh in $here first."
  if command -v "$tf" >/dev/null 2>&1; then
    :
  elif command -v tofu >/dev/null 2>&1; then
    tf=tofu
  else
    stop "Neither terraform nor tofu is on PATH. See docs/support-matrix.md."
  fi
  if grep -Eq '^prefix *= *""' "$here/$1/terraform.tfvars" 2>/dev/null; then
    stop "Fill in $1/terraform.tfvars first. Its plan names each value."
  fi
}

# init_if_needed: only a directory that also uses a registry provider needs
# init. The k0rdent build needs none.
init_if_needed() {
  if grep -q 'source *= *"hashicorp/' ./*.tf && [[ ! -d .terraform ]]; then
    run "$tf" init
  fi
}

up() {
  local status=0
  init_if_needed
  run "$tf" plan
  run "$tf" apply "$@"
  say "$tf plan -detailed-exitcode"
  "$tf" plan -detailed-exitcode || status=$?
  case "$status" in
    0) printf '\nNo changes after the apply. That is the result.\n' >&2 ;;
    2) stop "The plan after the apply proposes a change. That is a defect worth reporting." ;;
    *) stop "The plan after the apply failed." ;;
  esac
}

main() {
  local cmd="${1:-}" dir="${2:-}"
  case "$cmd" in
    setup) shift; setup "$@"; return ;;
    up | check | down) [[ -n "$dir" ]] || stop "Usage: $0 $cmd <directory>" ;;
    *) stop "Usage: $0 setup [ref] | up <directory> | check <directory> | down <directory>" ;;
  esac
  shift 2
  ready "$dir"
  say "cd $here/$dir"
  cd "$here/$dir"
  case "$cmd" in
    up) up "$@" ;;
    check) init_if_needed; run "$tf" plan ;;
    down) init_if_needed; run "$tf" destroy "$@" ;;
  esac
}

main "$@"
