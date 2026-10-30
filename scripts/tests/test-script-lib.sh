#!/usr/bin/env bash
#
# Tests scripts/lib/common.sh at its interface: what each helper prints, sets,
# refuses and hands to curl, with no script around it. The scripts that source
# it have their own tests. This one is here so a change to the library fails
# in one place, with the helper's name on it.
#
# Nothing here needs a network, a credential or a toolchain: curl and go are
# shims, and terraform and tofu are empty files on a PATH made for the test.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
lib="$here/scripts/lib/common.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "script lib test: FAIL - $1" >&2; exit 1; }

# in_lib <script> runs <script> in a fresh bash that has sourced the library,
# with stdout and stderr kept apart, and records its exit status in $status.
in_lib() {
  set +e
  bash -c ". \"$lib\"; $1" > "$work/stdout" 2> "$work/stderr"
  status=$?
  set -e
}

# A PATH holding only what each case puts there, plus the basics the library
# itself calls.
mkdir -p "$work/base"
for tool in bash env awk sed tr python3 cat cp mkdir dirname pwd; do
  ln -s "$(command -v "$tool")" "$work/base/$tool"
done

# --- sourcing --------------------------------------------------------------

in_lib '. "'"$lib"'"; echo sourced twice'
{ [ "$status" = 0 ] && grep -qx 'sourced twice' "$work/stdout"; } \
  || fail "sourcing the library twice did not succeed"

# Colours are off when stdout isn't a terminal, so a log file holds no escapes.
# shellcheck disable=SC2016 # the inner bash expands these
in_lib 'printf "[%s%s%s%s%s%s]" "$B" "$DIM" "$GREEN" "$YELLOW" "$RED" "$R"'
grep -qx '\[\]' "$work/stdout" || fail "colour codes were set with stdout not a terminal"

# --- output helpers --------------------------------------------------------

in_lib 'step one; note two; good three; warn four'
for want in '━━ one' '   two' '   ✓ three' '   ! four'; do
  grep -qF -- "$want" "$work/stdout" || fail "the output helpers did not print '$want'"
done

in_lib 'QUIET=1; step one; note two; good three; warn four; echo done'
[ "$(cat "$work/stdout")" = "done" ] || fail "QUIET=1 did not silence step, note, good and warn"

in_lib 'QUIET=1; die "it broke"; echo after'
[ "$status" = 1 ] || fail "die exited $status, not 1"
grep -qF '✗ it broke' "$work/stderr" || fail "die did not print its message on stderr"
! grep -q after "$work/stdout" || fail "die returned instead of exiting"

in_lib 'PATH=/nonexistent need surely-not-a-tool'
{ [ "$status" = 1 ] && grep -qF 'surely-not-a-tool is required' "$work/stderr"; } \
  || fail "need did not refuse a missing tool"

# --- find_terraform --------------------------------------------------------

mkdir -p "$work/both" "$work/tofu-only"
for d in both tofu-only; do printf '#!/bin/sh\n' > "$work/$d/tofu"; done
printf '#!/bin/sh\n' > "$work/both/terraform"
chmod +x "$work/both/terraform" "$work/both/tofu" "$work/tofu-only/tofu"

in_lib "PATH='$work/both'; find_terraform; echo \"\$TF_BIN\""
[ "$(cat "$work/stdout")" = terraform ] || fail "find_terraform did not prefer terraform"
in_lib "PATH='$work/tofu-only'; find_terraform; echo \"\$TF_BIN\""
[ "$(cat "$work/stdout")" = tofu ] || fail "find_terraform did not fall back to tofu"
in_lib "PATH='$work/both'; TF_BIN=/opt/my/tf; find_terraform; echo \"\$TF_BIN\""
[ "$(cat "$work/stdout")" = /opt/my/tf ] || fail "find_terraform replaced a TF_BIN the caller set"
in_lib "PATH='$work/base'; find_terraform"
{ [ "$status" = 1 ] && grep -q 'neither terraform nor tofu' "$work/stderr"; } \
  || fail "find_terraform did not say neither CLI is on PATH"

# --- read_names, build_provider ---------------------------------------------

# A go that answers the name audit and builds an empty binary where -o says.
mkdir -p "$work/gobin" "$work/tree/repo"
cat > "$work/gobin/go" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  run)   printf 'Provider type name             : my-prov\nRegistry namespace             : my-ns\n' ;;
  env)   [ "$2" = GOOS ] && echo testos || echo testarch ;;
  build) while [ $# -gt 0 ]; do [ "$1" = -o ] && { : > "$2"; break; }; shift; done ;;
esac
EOF
chmod +x "$work/gobin/go"
GO_PATH="$work/gobin:$work/base"

in_lib "PATH='$GO_PATH'; REPO='$work/tree/repo'; read_names; echo \"\$PROVIDER_NAME \$NAMESPACE \$ENV_PREFIX\""
[ "$(cat "$work/stdout")" = "my-prov my-ns MY_PROV_" ] \
  || fail "read_names set '$(cat "$work/stdout")', not 'my-prov my-ns MY_PROV_'"

in_lib "PATH='$GO_PATH'; REPO='$work/tree/repo'; read_names; build_provider '$work/run' 9.9.9-test; echo \"\$PROVIDER_BIN_DIR\""
bindir="$work/run/plugins/my-ns/my-prov/9.9.9-test/testos_testarch"
[ "$(tail -1 "$work/stdout")" = "$bindir" ] || fail "build_provider set PROVIDER_BIN_DIR to '$(tail -1 "$work/stdout")'"
[ -f "$bindir/terraform-provider-my-prov" ] || fail "build_provider did not build into $bindir"
grep -qF "\"my-ns/my-prov\" = \"$bindir\"" "$work/run/terraform.rc" \
  || fail "build_provider's terraform.rc does not point the override at the binary"

in_lib "PATH='$GO_PATH'; REPO='$work/tree/repo'; read_names; build_provider_mirror '$work/mirrored' 9.9.9-test"
for host in registry.terraform.io registry.opentofu.org; do
  [ -f "$work/mirrored/mirror/$host/my-ns/my-prov/9.9.9-test/testos_testarch/terraform-provider-my-prov_v9.9.9-test" ] \
    || fail "build_provider_mirror did not put the binary in the mirror for $host"
done
grep -qF "path    = \"$work/mirrored/mirror\"" "$work/mirrored/terraform.rc" \
  || fail "build_provider_mirror's terraform.rc does not install from the mirror"
grep -qF 'exclude = ["registry.terraform.io/my-ns/my-prov", "registry.opentofu.org/my-ns/my-prov"]' "$work/mirrored/terraform.rc" \
  || fail "build_provider_mirror's terraform.rc lets direct install the provider too"

# --- the fake: start_loopback_server and export_fake_credential -----------

# A stand-in for fakeapi: it writes its secrets file and prints a URL, the two
# things start_loopback_server waits for, then stays up until it's killed.
cat > "$work/fakeserver" <<'EOF'
#!/usr/bin/env bash
[ "$1" = -secrets-file ] || { echo "wrong flag $1" >&2; exit 2; }
[ ! -e "$2" ] || { echo "the secrets file was already there" >&2; exit 2; }
printf "FAKE_CLIENT_ID='sa@fake'\nFAKE_CLIENT_SECRET='k0r_fake_secret_abc'\nFAKE_OWNER_TOKEN='own'\n" > "$2"
echo "http://127.0.0.1:4242"
exec sleep 30
EOF
chmod +x "$work/fakeserver"
mkdir -p "$work/srv"
echo stale > "$work/srv/secrets.env"
in_lib "start_loopback_server '$work/fakeserver' -secrets-file '$work/srv/secrets.env' '$work/srv/url' '$work/srv/log'
        echo \"\$SERVER_URL \$SERVER_PID\"; kill \"\$SERVER_PID\""
[ "$status" = 0 ] || fail "start_loopback_server did not start the server: $(cat "$work/srv/log")"
read -r url pid < "$work/stdout"
[ "$url" = http://127.0.0.1:4242 ] || fail "start_loopback_server set SERVER_URL to '$url'"
[ -n "$pid" ] || fail "start_loopback_server did not set SERVER_PID"

# A server that never prints its URL is a failure, not a hang.
printf '#!/usr/bin/env bash\nexit 1\n' > "$work/deadserver"
chmod +x "$work/deadserver"
in_lib "start_loopback_server '$work/deadserver' -secrets-file '$work/srv/dead.env' '$work/srv/dead.url' '$work/srv/dead.log'"
[ "$status" != 0 ] || fail "start_loopback_server succeeded for a server that never started"

in_lib "ENV_PREFIX=MY_PROV_; export_fake_credential '$work/run' '$work/srv/secrets.env'; env"
grep -qx "TF_CLI_CONFIG_FILE=$work/run/terraform.rc" "$work/stdout" \
  || fail "export_fake_credential did not export the CLI configuration"
{ grep -qx 'MY_PROV_CLIENT_ID=sa@fake' "$work/stdout" && grep -qx 'MY_PROV_CLIENT_SECRET=k0r_fake_secret_abc' "$work/stdout"; } \
  || fail "export_fake_credential did not export the fake's credential under the provider's prefix"
in_lib "ENV_PREFIX=MY_PROV_; export_fake_credential '$work/run' '$work/srv/missing.env'"
[ "$status" = 1 ] || fail "export_fake_credential carried on without a secrets file"

# --- private_dir and private_tmpdir ---------------------------------------

mode() { python3 -c 'import os, stat, sys; print(oct(stat.S_IMODE(os.lstat(sys.argv[1]).st_mode)))' "$1"; }

in_lib "private_dir '$work/priv/a/b'"
[ "$status" = 0 ] && [ "$(mode "$work/priv/a/b")" = 0o700 ] \
  || fail "private_dir did not make a 0700 directory: status $status, $(cat "$work/stderr")"

# A run directory a previous version left 0755 is tightened, not trusted.
mkdir -p "$work/loose" && chmod 755 "$work/loose"
in_lib "private_dir '$work/loose'"
[ "$status" = 0 ] && [ "$(mode "$work/loose")" = 0o700 ] || fail "private_dir did not tighten a 0755 directory"

# Another user can plant either of these first under a shared /tmp.
mkdir -p "$work/real"
ln -s "$work/real" "$work/planted"
in_lib "private_dir '$work/planted'"
[ "$status" = 1 ] && grep -q 'symlink' "$work/stderr" || fail "private_dir accepted a symlink"
in_lib "private_dir /usr"
[ "$status" = 1 ] && grep -q 'another user' "$work/stderr" || fail "private_dir accepted a directory another user owns"

# The checks have to hold for what mkdir -p leaves, not only for what was
# there before it. Another user who wins the race between the first check and
# mkdir plants a symlink there, which mkdir -p takes as the directory. The
# shim plays that user: it plants the link in place of making the directory.
mkdir -p "$work/real2"
in_lib "mkdir() { ln -s '$work/real2' '$work/raced'; }; private_dir '$work/raced'"
[ "$status" = 1 ] && grep -q 'symlink' "$work/stderr" || fail "private_dir accepted a symlink planted while it ran mkdir"

# And it says the directory is private only once it is: a chmod that didn't
# take, on a filesystem that ignores modes, leaves it as it was.
mkdir -p "$work/stays-open" && chmod 755 "$work/stays-open"
in_lib "chmod() { :; }; private_dir '$work/stays-open'"
[ "$status" = 1 ] && grep -q '0700' "$work/stderr" || fail "private_dir accepted a directory still open to other users"

in_lib 'a="$(private_tmpdir)" && b="$(private_tmpdir)" && echo "$a" && echo "$b"'
a="$(sed -n 1p "$work/stdout")"; b="$(sed -n 2p "$work/stdout")"
[ "$status" = 0 ] && [ -d "$a" ] && [ -d "$b" ] && [ "$a" != "$b" ] || fail "private_tmpdir did not make two directories"
[ "$(mode "$a")" = 0o700 ] || fail "private_tmpdir made a $(mode "$a") directory"
rmdir "$a" "$b"

# --- endpoint_host and endpoint_is_encrypted -------------------------------

while read -r url want; do
  in_lib "endpoint_host '$url'"
  [ "$(cat "$work/stdout")" = "$want" ] || fail "endpoint_host $url printed '$(cat "$work/stdout")', not $want"
done <<'EOF'
https://api.example.com api.example.com
http://localhost:8000/v1/regions localhost:8000
HTTPS://Example.COM:443/ Example.COM:443
EOF

for ok in https://example.invalid http://127.0.0.1:1 http://localhost:1 "http://[::1]:1" \
          "http://[::ffff:127.0.0.1]:1" http://127.8.9.10; do
  in_lib "endpoint_is_encrypted '$ok'"
  [ "$status" = 0 ] || fail "endpoint_is_encrypted refused $ok, which the provider accepts"
done
for bad in http://example.invalid http://localhost.example.invalid http://127.0.0.1.example.invalid \
           http://LOCALHOST:1 "http://[::1%25lo0]:1" http://127.0.0.1@example.invalid:1 \
           ftp://127.0.0.1 " https://example.invalid" "" not-a-url; do
  in_lib "endpoint_is_encrypted '$bad'"
  [ "$status" != 0 ] || fail "endpoint_is_encrypted accepted '$bad', which the provider refuses"
done

# --- token_request_body ----------------------------------------------------

# A secret with every character a shell or JSON could trip on still comes
# back exactly, and never as an argument to python3.
nasty="s3'cr\"et \$(touch $work/pwned) \\ \`x\`"
in_lib "token_request_body id@vendor '$(printf '%s' "$nasty" | sed "s/'/'\\\\''/g")' ''"
got="$(python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["grant_type"], d["client_id"], d["client_secret"], sep="|")' < "$work/stdout")"
[ "$got" = "client_credentials|id@vendor|$nasty" ] || fail "token_request_body mangled a client secret: $got"
[ ! -e "$work/pwned" ] || fail "part of a client secret ran as shell"

in_lib "token_request_body '' '' k0r_key"
got="$(python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["grant_type"], d["api_key"], sorted(d), sep="|")' < "$work/stdout")"
[ "$got" = "api_key|k0r_key|['api_key', 'grant_type']" ] || fail "token_request_body built the wrong api_key body: $got"

# --- curl_bearer -----------------------------------------------------------

mkdir -p "$work/curlbin"
cat > "$work/curlbin/curl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$@" > "$work/curl.argv"
cat > "$work/curl.stdin"
EOF
chmod +x "$work/curlbin/curl"
in_lib "PATH='$work/curlbin:$work/base'; curl_bearer dummy-bearer-not-a-secret -sS https://example.invalid/x"
[ "$status" = 0 ] || fail "curl_bearer failed against the shim: $(cat "$work/stderr")"
if grep -q dummy-bearer-not-a-secret "$work/curl.argv"; then
  fail "curl_bearer put the bearer in curl's argv"
fi
grep -qx 'Authorization: Bearer dummy-bearer-not-a-secret' "$work/curl.stdin" \
  || fail "curl_bearer did not hand curl the bearer on stdin"
[ "$(tr '\n' ' ' < "$work/curl.argv")" = "-H @- -sS https://example.invalid/x " ] \
  || fail "curl_bearer passed curl '$(tr '\n' ' ' < "$work/curl.argv")'"

# --- walkthrough.sh: walkthrough_dirs, walkthrough_pick, walkthrough_tfvars --

wlib="$here/scripts/lib/walkthrough.sh"
ex="$work/examples"
mkdir -p "$ex/look" "$ex/nodes" "$ex/full" "$ex/zone" "$ex/notes"
for d in look nodes full zone; do : > "$ex/$d/main.tf"; done
cat > "$ex/full/terraform.tfvars" <<'EOF'
prefix       = "" # yours
environment  = "dev"
cluster_type = ""
build = {
  network = true # on
  file    = false # off
}
EOF
printf 'prefix = ""\nzone_id = "" # new\n' > "$ex/zone/terraform.tfvars"

# in_wlib <script> is in_lib with walkthrough.sh sourced too.
in_wlib() { in_lib ". \"$wlib\"; $1"; }

in_wlib "walkthrough_dirs '$ex' | tr '\n' ' '"
[ "$(cat "$work/stdout")" = "look full full-everything zone " ] \
  || fail "walkthrough_dirs listed '$(cat "$work/stdout")', not 'look full full-everything zone '"

cat > "$work/look.json" <<'EOF'
{"cluster_types": {"value": {"r0": [], "r1": ["cpu-general", "gpu"]}},
 "projects": {"value": ["p0 in org, region r0 x", "p1 in org, region r1 x"]},
 "organizations": {"value": ["o1 the org"]}}
EOF
in_wlib "walkthrough_pick '$work/look.json'; echo \"\$region \$cluster_type \$filtered_type \$organization \$project\""
[ "$(cat "$work/stdout")" = "r1 cpu-general gpu o1 p1" ] \
  || fail "walkthrough_pick set '$(cat "$work/stdout")', not 'r1 cpu-general gpu o1 p1'"

in_wlib "region=r1 cluster_type=t1 filtered_type=t2; walkthrough_tfvars full '$ex' ci"
{ grep -qx 'prefix = "ci"' "$work/stdout" && grep -qx 'cluster_type = "t2"' "$work/stdout" &&
  grep -qx 'environment  = "dev"' "$work/stdout" && grep -q '^  file    = false' "$work/stdout"; } \
  || fail "walkthrough_tfvars full did not fill in the values and keep the rest: $(cat "$work/stdout")"
in_wlib "region=r1 cluster_type=t1 filtered_type=t2; walkthrough_tfvars full-everything '$ex' ci"
grep -q '^  file    = true' "$work/stdout" || fail "walkthrough_tfvars full-everything left a switch off"
in_wlib "walkthrough_tfvars zone '$ex' ci"
{ [ "$status" != 0 ] && grep -qF 'zone/terraform.tfvars needs zone_id' "$work/stderr"; } \
  || fail "walkthrough_tfvars did not refuse a variable it doesn't fill in"
in_wlib "walkthrough_tfvars missing '$ex' ci"
{ [ "$status" != 0 ] && grep -qF 'no directory missing' "$work/stderr"; } \
  || fail "walkthrough_tfvars did not refuse a directory that isn't there"
in_wlib "walkthrough_tfvars look '$ex' ci; echo finished"
[ "$(cat "$work/stdout")" = finished ] || fail "walkthrough_tfvars printed a terraform.tfvars for a directory without one"

echo "script lib test: PASS"
