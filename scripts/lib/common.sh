# shellcheck shell=bash
#
# Helpers shared by the scripts under scripts/ and ci/. Sourced, never run:
#
#   . "$REPO/scripts/lib/common.sh"
#
# read_names and build_provider need REPO set to the repository root. Nothing
# here changes directory or sets shell options. scripts/tests/test-script-lib.sh
# tests it.

# Sourcing twice is harmless.
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

# private_dir <path> makes <path> a directory only this user can enter, and
# refuses a symlink or another user's directory, which anyone can plant first
# under a shared /tmp. The checks run again after mkdir -p, since a symlink
# planted in between is taken as the directory. The mode is read back because
# a filesystem that ignores modes lets chmod succeed and change nothing.
private_dir() {
  local dir="$1"
  [ ! -L "$dir" ] || die "$dir is a symlink. Refusing to use it as a private directory; remove it, or point at another path."
  (umask 077 && mkdir -p "$dir") || die "could not create $dir"
  [ ! -L "$dir" ] || die "$dir became a symlink while it was being created. Refusing to use it as a private directory; remove it, or point at another path."
  [ -d "$dir" ] || die "$dir isn't a directory. Refusing to use it as a private directory; point at another path."
  [ -O "$dir" ] || die "$dir belongs to another user. Refusing to use it as a private directory; point at another path."
  chmod 700 "$dir" || die "could not make $dir private"

  # Owner rwx and no group or other bit, in a form BSD and GNU find both take.
  # A setgid bit is not checked: GNU chmod keeps it on a directory and it
  # opens nothing.
  [ -n "$(find "$dir" -maxdepth 0 -perm -700 ! -perm -040 ! -perm -020 ! -perm -010 \
          ! -perm -004 ! -perm -002 ! -perm -001)" ] \
    || die "$dir is still open to other users after chmod, not 0700. Refusing to use it as a private directory; point at another path."
}

# private_tmpdir prints the path of a new directory only this user can enter.
# Call it as `dir="$(private_tmpdir)" || exit 1`.
private_tmpdir() {
  local dir
  dir="$(mktemp -d)" || die "could not create a temporary directory"
  chmod 700 "$dir" || die "could not make $dir private"
  printf '%s' "$dir"
}

# read_names sets PROVIDER_NAME, NAMESPACE and ENV_PREFIX from the Go name
# audit, so no script becomes another place a rename has to find.
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

# build_provider <run dir> <version> builds the provider under <run dir>/plugins,
# sets PROVIDER_BIN_DIR to its directory, and writes <run dir>/terraform.rc with
# a dev_overrides block that points Terraform at it.
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

# start_loopback_server <binary> <flag> <secrets file> <url file> <log file>
# starts fakeapi or statebackend from internal/tools and waits for it to print
# its URL. It sets SERVER_PID and SERVER_URL, and fails if the server didn't
# come up. The server refuses a secrets file that already exists, so it's
# removed first; the caller names a path in its own private run directory.
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

# export_fake_credential <run dir> <secrets file> exports the fake's credential
# for this run and the CLI configuration build_provider wrote. The provider
# reads credentials from the environment only. It also sets the FAKE_*
# variables the file holds, such as FAKE_OWNER_TOKEN.
export_fake_credential() {
  # shellcheck source=/dev/null # written by internal/tools/fakeapi
  . "$2" || die "could not read the fake's secrets from $2"
  export "${ENV_PREFIX}CLIENT_ID=$FAKE_CLIENT_ID"
  export "${ENV_PREFIX}CLIENT_SECRET=$FAKE_CLIENT_SECRET"
  export TF_CLI_CONFIG_FILE="$1/terraform.rc"
  export TF_IN_AUTOMATION=1
}

# endpoint_host prints the host of a URL, port included, which is what
# LIVE_CONFIRM must match.
endpoint_host() {
  printf '%s' "$1" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||' -e 's|/.*$||'
}

# endpoint_is_encrypted succeeds for an https URL, and for an http one only when
# its host is literally localhost or a loopback IP, with no DNS lookup. It
# copies isLoopbackHost in internal/provider/config.go, because the scripts
# send the credential before the provider sees the endpoint. Like Go's
# url.Parse, it doesn't lowercase the host, so http://LOCALHOST and
# http://[::1%25lo0] fail.
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
# grant otherwise. The values reach python3 through its environment, not argv,
# which any user can read from ps.
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
# <token>", handed in on stdin (`-H @-`) so the token is never in argv. That
# takes curl's stdin, so a request body has to be --data or a file, not
# --data-binary @-.
curl_bearer() {
  local token="$1"; shift
  printf 'Authorization: Bearer %s\n' "$token" | curl -H @- "$@"
}
