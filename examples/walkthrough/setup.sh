# shellcheck shell=bash
#
# Sets the environment the provider reads, in your current shell. Works in
# bash and zsh.
#
# Usage: source it, don't run it.
#
#   . ./setup.sh
#
# It asks for the landscape's API endpoint and a service account's client id
# and secret. The secret is read without echoing, so it never lands in your
# shell history, a .tf file or a plan. Source it again to change any of them.
#
# Needs: a provider build. In a run folder from walkthrough.sh setup, or one
# made by hand as docs/walkthrough-hardware.md shows, that's the build in the
# folder's terraform.rc, one level up from this file. Otherwise it's the one
# docs/build-from-source.md makes. It says which build it points Terraform at,
# and what TF_CLI_CONFIG_FILE was if that changes it.
#
# Status: 0 with the environment set. 1 with a message on stderr when there is
# no provider build, input ends early, or a value is still missing.
#
# It runs in your shell, so it never sets shell options or exits. Its work is
# in one function with local variables, removed again before it returns.

k0rdent_setup() {
  local rc_file answer run_dir

  # The provider build. Without it, Terraform looks for k0rdent/k0rdent in a
  # registry and fails. A run folder's own build comes first.
  run_dir="$(cd "$(dirname "$1")/.." && pwd)"
  if [[ -f "$run_dir/terraform.rc" ]]; then
    rc_file="$run_dir/terraform.rc"
  else
    rc_file="${XDG_CACHE_HOME:-$HOME/.cache}/k0rdent-dev/terraform.rc"
  fi
  if [[ ! -f "$rc_file" ]]; then
    printf 'No provider build at %s. Follow docs/walkthrough-hardware.md step 1 first.\n' "$rc_file" >&2
    return 1
  fi
  if [[ -n "${TF_CLI_CONFIG_FILE:-}" && "$TF_CLI_CONFIG_FILE" != "$rc_file" ]]; then
    printf 'TF_CLI_CONFIG_FILE was %s, now %s.\n' "$TF_CLI_CONFIG_FILE" "$rc_file" >&2
  fi
  export TF_CLI_CONFIG_FILE="$rc_file"
  printf 'Provider build: %s, from %s.\n' \
    "$(sed -n 's/.*"k0rdent\/k0rdent" *= *"\([^"]*\)".*/\1/p' "$rc_file")" "$rc_file" >&2

  # Enter on any prompt keeps the value already set.
  printf 'API endpoint, such as https://api.example.k0rdent.ai [%s]: ' "${K0RDENT_ENDPOINT:-}" >&2
  read -r answer || return 1
  if [[ -n "$answer" ]]; then
    export K0RDENT_ENDPOINT="$answer"
  fi

  printf 'Client id [%s]: ' "${K0RDENT_CLIENT_ID:-}" >&2
  read -r answer || return 1
  if [[ -n "$answer" ]]; then
    export K0RDENT_CLIENT_ID="$answer"
  fi

  printf 'Client secret (not shown, Enter keeps the current one): ' >&2
  read -rs answer || return 1
  printf '\n' >&2
  if [[ -n "$answer" ]]; then
    export K0RDENT_CLIENT_SECRET="$answer"
  fi

  if [[ -z "${K0RDENT_ENDPOINT:-}" || -z "${K0RDENT_CLIENT_ID:-}" || -z "${K0RDENT_CLIENT_SECRET:-}" ]]; then
    printf 'The endpoint, client id and secret are all needed.\n' >&2
    return 1
  fi

  # Left over from another landscape. The .tf files set the region and the
  # project themselves.
  unset K0RDENT_API_KEY K0RDENT_REGION K0RDENT_PROJECT

  printf 'Set for %s as %s.\n' "$K0RDENT_ENDPOINT" "$K0RDENT_CLIENT_ID" >&2
}

# This file's own path: bash names it in BASH_SOURCE, zsh in $0.
if k0rdent_setup "${BASH_SOURCE[0]:-$0}"; then
  unset -f k0rdent_setup
else
  unset -f k0rdent_setup
  return 1
fi
