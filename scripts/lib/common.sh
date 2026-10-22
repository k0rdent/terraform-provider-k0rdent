# shellcheck shell=bash
#
# The helpers the scripts under scripts/ and ci/ had each been carrying a copy
# of. Sourced, never run:
#
#   . "$REPO/scripts/lib/common.sh"
#
# The copies had started to drift. Three scripts carried the same endpoint
# check "byte for byte" with a comment asking whoever changed one to change the
# others, and the error colour, the client module message and the way a
# bearer reached curl differed from script to script for no reason anyone
# wrote down. One file means one place to fix, and scripts/tests/test-script-lib.sh
# tests it at this interface rather than through each caller.
#
# The functions that read the tree (read_names, build_provider) need REPO set
# to the repository root. Nothing here changes
# directory or sets shell options, so a caller's `set -e` and `set -u` are
# its own.

# Sourcing twice is harmless, which matters when one script sources another's
# helpers after sourcing this itself.
if [ -n "${K0RDENT_SCRIPTS_LIB:-}" ]; then
  return 0
fi
K0RDENT_SCRIPTS_LIB=1

# Colours, off when the output isn't a terminal so a piped run stays readable.
# shellcheck disable=SC2034 # read by the scripts that source this
if [ -t 1 ]; then
  B=$'\033[1m'; DIM=$'\033[2m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; RED=$'\033[31m'; R=$'\033[0m'
else
  B=''; DIM=''; GREEN=''; YELLOW=''; RED=''; R=''
fi

# QUIET=1 silences everything but die, for a caller whose stdout goes
# straight into an eval.
step() { [ "${QUIET:-0}" = 1 ] || printf '\n%s━━ %s %s\n' "$B" "$1" "$R"; }
note() { [ "${QUIET:-0}" = 1 ] || printf '%s   %s%s\n' "$DIM" "$1" "$R"; }
good() { [ "${QUIET:-0}" = 1 ] || printf '%s   ✓ %s%s\n' "$GREEN" "$1" "$R"; }
warn() { [ "${QUIET:-0}" = 1 ] || printf '%s   ! %s%s\n' "$YELLOW" "$1" "$R"; }
die()  { printf '\n%s   ✗ %s%s\n\n' "$RED" "$1" "$R" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not on PATH"; }

# ---------------------------------------------------------------------------
# Private directories
# ---------------------------------------------------------------------------

# private_dir <path> makes <path> a directory only this user can read, write
# or enter, and refuses one it can't trust. The run directories hold a
# provider binary that Terraform runs, a CLI configuration that says which
# binary that is, Terraform state, logs, and the fake's secrets. Under a
# shared parent such as /tmp, or a DEMO_RUN_DIR someone points there, another
# user can create the path first: as a symlink to somewhere they read, or as
# a directory they own and can swap files in. Either is refused rather than
# used, and a directory of this user's that a previous version left 0755 is
# tightened.
#
# The checks run again after mkdir -p, on what it left. A symlink planted
# between the first check and mkdir is one mkdir -p takes as the directory,
# and -O and chmod both follow it. And the mode is read back after chmod,
# since a filesystem that ignores modes lets chmod succeed and changes
# nothing.
private_dir() {
  local dir="$1"
  [ ! -L "$dir" ] || die "$dir is a symlink. Refusing to use it as a private directory; remove it, or point at another path."
  (umask 077 && mkdir -p "$dir") || die "could not create $dir"
  [ ! -L "$dir" ] || die "$dir became a symlink while it was being created. Refusing to use it as a private directory; remove it, or point at another path."
  [ -d "$dir" ] || die "$dir isn't a directory. Refusing to use it as a private directory; point at another path."
  [ -O "$dir" ] || die "$dir belongs to another user. Refusing to use it as a private directory; point at another path."
  chmod 700 "$dir" || die "could not make $dir private"
  # Owner rwx and no group or other bit, written so BSD and GNU find both
  # take it. A setgid bit a parent passed down is left out of it, since GNU
  # chmod keeps that bit on a directory and it opens nothing.
  [ -n "$(find "$dir" -maxdepth 0 -perm -700 ! -perm -040 ! -perm -020 ! -perm -010 \
          ! -perm -004 ! -perm -002 ! -perm -001)" ] \
    || die "$dir is still open to other users after chmod, not 0700. Refusing to use it as a private directory; point at another path."
}

# private_tmpdir prints the path of a new directory only this user can enter.
# mktemp -d picks a name nobody can guess and already makes it 0700 on Linux
# and macOS. The chmod says so where it matters rather than relying on it.
# Call it as `dir="$(private_tmpdir)" || exit 1`.
private_tmpdir() {
  local dir
  dir="$(mktemp -d)" || die "could not create a temporary directory"
  chmod 700 "$dir" || die "could not make $dir private"
  printf '%s' "$dir"
}

# ---------------------------------------------------------------------------
# The tree: the provider's name, Terraform, the client, the binary
# ---------------------------------------------------------------------------

# read_names sets PROVIDER_NAME, NAMESPACE and ENV_PREFIX. The provider name is
# provisional and lives in one Go constant. Read it rather than writing it
# out, so no script becomes another place a rename has to find.
read_names() {
  local audit
  audit="$(cd "$REPO" && go run ./internal/tools/nameaudit)" || die "could not run the name audit"
  PROVIDER_NAME="$(printf '%s\n' "$audit" | awk -F': *' '/^Provider type name/ {print $2}')"
  NAMESPACE="$(printf '%s\n' "$audit" | awk -F': *' '/^Registry namespace/ {print $2}')"
  ENV_PREFIX="$(printf '%s' "$PROVIDER_NAME" | tr '[:lower:]-' '[:upper:]_')_"
  [ -n "$PROVIDER_NAME" ] && [ -n "$NAMESPACE" ] || die "could not read the provider name from the name audit"
}

# find_terraform sets TF_BIN, keeping one the caller already set. It prefers
# terraform and falls back to tofu.
find_terraform() {
  if [ -n "${TF_BIN:-}" ]; then return; fi
  if command -v terraform >/dev/null 2>&1; then
    TF_BIN=terraform
  elif command -v tofu >/dev/null 2>&1; then
    TF_BIN=tofu
  else
    die "neither terraform nor tofu is on PATH. On macOS: brew install terraform"
  fi
}

# build_provider <run dir> <version> builds the provider under <run dir>/plugins
# and writes <run dir>/terraform.rc pointing Terraform at it. It sets
# PROVIDER_BIN_DIR to the directory the binary is in.
#
# dev_overrides makes Terraform run the binary straight out of that directory,
# with no registry, no download and no lock file. It's the right tool here and
# it's noisy about it: every command warns that an override is in effect.
build_provider() {
  local run_dir="$1" version="$2" arch
  arch="$(go env GOOS)_$(go env GOARCH)"
  PROVIDER_BIN_DIR="$run_dir/plugins/$NAMESPACE/$PROVIDER_NAME/$version/$arch"
  mkdir -p "$PROVIDER_BIN_DIR"
  (cd "$REPO" && go build -ldflags "-X main.version=$version" \
    -o "$PROVIDER_BIN_DIR/terraform-provider-$PROVIDER_NAME" .) \
    || die "could not build the provider"
  good "built terraform-provider-$PROVIDER_NAME"

  cat > "$run_dir/terraform.rc" <<EOF
provider_installation {
  dev_overrides {
    "$NAMESPACE/$PROVIDER_NAME" = "$PROVIDER_BIN_DIR"
  }
  direct {}
}
EOF
}

# ---------------------------------------------------------------------------
# The fake control plane and the state backend
# ---------------------------------------------------------------------------

# start_loopback_server <binary> <flag> <secrets file> <url file> <log file>
# starts one of the test servers in internal/tools (fakeapi or statebackend)
# and waits for it to print its URL. It sets SERVER_PID and SERVER_URL, and
# fails if the server didn't come up.
#
# Each server makes a secret for the run and writes it, 0600, to the file its
# <flag> (-secrets-file or -password-file) names, which has to not exist yet.
# So this removes the file first. That's safe because the caller names a path
# in its own private run directory.
start_loopback_server() {
  local bin="$1" flag="$2" secrets="$3" url_file="$4" log_file="$5" _
  rm -f "$secrets"
  : > "$url_file"
  : > "$log_file"
  "$bin" "$flag" "$secrets" > "$url_file" 2> "$log_file" &
  # shellcheck disable=SC2034 # read by the caller
  SERVER_PID=$!
  for _ in $(seq 1 100); do
    [ -s "$url_file" ] && [ -s "$secrets" ] && break
    sleep 0.1
  done
  SERVER_URL="$(cat "$url_file")"
  [ -n "$SERVER_URL" ] && [ -s "$secrets" ]
}

# export_fake_credential <run dir> <secrets file> exports the fake control
# plane's credential, read from the secrets file the fake wrote for this run,
# and the CLI configuration build_provider wrote. It's exported rather than
# written to a file because the provider reads credentials from the
# environment only, with no `client_secret` attribute and no credentials file,
# by design. And one of the things the conformance suite measures is that no
# file in the working directory could have supplied it. It also sets the
# FAKE_* variables the file holds, among them FAKE_CLIENT_SECRET_NEXT for a
# rotation and FAKE_OWNER_TOKEN for the fake's /_harness routes.
export_fake_credential() {
  # shellcheck source=/dev/null # written by internal/tools/fakeapi
  . "$2" || die "could not read the fake's secrets from $2"
  export "${ENV_PREFIX}CLIENT_ID=$FAKE_CLIENT_ID"
  export "${ENV_PREFIX}CLIENT_SECRET=$FAKE_CLIENT_SECRET"
  export TF_CLI_CONFIG_FILE="$1/terraform.rc"
  export TF_IN_AUTOMATION=1
}

# ---------------------------------------------------------------------------
# Endpoints and credentials
# ---------------------------------------------------------------------------

# endpoint_host prints the host of a URL, port included, which is what
# LIVE_CONFIRM must match.
endpoint_host() {
  printf '%s' "$1" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|/.*$||'
}

# endpoint_is_encrypted succeeds for an https URL, and for an http one only when
# its host is literally localhost or a loopback IP. It's the provider's own
# rule (isLoopbackHost in internal/provider/config.go), and it has to run in
# the scripts as well because they mint with the credential, or send a bearer,
# before the provider ever sees the endpoint. No DNS: a name that resolves to
# 127.0.0.1 today can resolve elsewhere tomorrow. The host is read the way Go's
# url.Parse reads it, so it isn't lowercased and an IPv6 zone isn't a loopback
# IP. That means http://LOCALHOST and http://[::1%25lo0] fail here as they do
# in the provider.
endpoint_is_encrypted() {
  ENDPOINT_TO_CHECK="$1" python3 -c '
import ipaddress, os, re, sys, urllib.parse
raw = os.environ["ENDPOINT_TO_CHECK"]
if raw != raw.strip() or re.search("[\x00-\x1f\x7f]", raw):
    sys.exit(1)
try:
    u = urllib.parse.urlsplit(raw)
except ValueError:
    sys.exit(1)
m = re.fullmatch(r"\[([^]]*)\](?::[0-9]*)?|([^[\]:]*)(?::[0-9]*)?", u.netloc.rpartition("@")[2])
if not m or not u.netloc or u.scheme not in ("http", "https"):
    sys.exit(1)
if u.scheme == "https":
    sys.exit(0)
host = m.group(1) if m.group(1) is not None else m.group(2)
if host == "localhost":
    sys.exit(0)
if "%" in host:
    sys.exit(1)
try:
    ip = ipaddress.ip_address(host)
except ValueError:
    sys.exit(1)
ip = getattr(ip, "ipv4_mapped", None) or ip
sys.exit(0 if ip.is_loopback else 1)'
}

# token_request_body <client id> <client secret> <api key> prints the JSON body
# of a token mint: client_credentials when a client id is given, the api_key
# grant otherwise. The values reach python3 through its environment, which
# only this user can read, and never through argv, which every user can.
token_request_body() {
  if [ -n "$1" ]; then
    CID="$1" CSEC="$2" python3 -c '
import json, os
print(json.dumps({"grant_type": "client_credentials",
                  "client_id": os.environ["CID"],
                  "client_secret": os.environ["CSEC"]}))'
  else
    AKEY="$3" python3 -c '
import json, os
print(json.dumps({"grant_type": "api_key", "api_key": os.environ["AKEY"]}))'
  fi
}

# curl_bearer <token> [curl args...] runs curl with "Authorization: Bearer
# <token>" as a header. The header goes in on stdin (`-H @-`, curl 7.55 and
# later), not in argv, because any user on the machine can read a process's
# argv from ps. printf is a builtin, so the token is never an argument to
# anything. It takes curl's stdin, so a request body has to be an argument
# (--data) or a file, not --data-binary @-.
curl_bearer() {
  local token="$1"; shift
  printf 'Authorization: Bearer %s\n' "$token" | curl -H @- "$@"
}
