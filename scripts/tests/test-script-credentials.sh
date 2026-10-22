#!/usr/bin/env bash
#
# Proves the scripts that mint with a real credential keep it on https and off
# the command line. Nothing here needs a network or a real credential: curl and
# go are shims that record what they were handed, every credential is a dummy,
# and the one real HTTP exchange is between two servers on 127.0.0.1.
#
# Each script runs from a copy in a throwaway tree, so `cd "$(dirname "$0")/.."`
# lands there and nothing the script writes touches this checkout.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "script credentials test: FAIL - $1" >&2; exit 1; }

DUMMY_KEY="dummy-api-key-not-a-secret"
DUMMY_BEARER="dummy-bearer-not-a-secret"

# A curl that never opens a socket. Each call writes its argv and its stdin to
# the log directory, and answers with a token for a mint and an empty list for
# anything else.
mkdir -p "$work/bin"
cat > "$work/bin/curl" <<EOF
#!/usr/bin/env bash
n="\$(ls "$work/calls" 2>/dev/null | grep -c '^argv\.' || true)"
mkdir -p "$work/calls"
printf '%s\n' "\$@" > "$work/calls/argv.\$n"
cat > "$work/calls/stdin.\$n"
case "\$*" in
  *http_code*)   printf '200' ;;
  */auth/token*) printf '{"access_token":"$DUMMY_BEARER"}' ;;
  *)             printf '{"regions":[],"clusterTypes":[]}' ;;
esac
EOF
# live.sh reads the provider name from the Go name audit and builds the
# provider. Neither needs the real toolchain to answer.
cat > "$work/bin/go" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  run)   printf 'Provider type name             : k0rdent\nRegistry namespace             : k0rdent\n' ;;
  env)   echo test ;;
  build) while [ $# -gt 0 ]; do [ "$1" = -o ] && { : > "$2"; break; }; shift; done ;;
  *)     echo test ;;
esac
EOF
chmod +x "$work/bin/curl" "$work/bin/go"

# A tree shaped the way live.sh expects: the script and the live example.
setup() {
  rm -rf "$work/repo" "$work/calls"
  mkdir -p "$work/repo/scripts" "$work/repo/examples/live/storage" "$work/calls"
  cp "$here/scripts/live.sh" "$here/scripts/live-storage.sh" "$here/scripts/capture-live-fixtures.sh" \
     "$work/repo/scripts/"
  cp -R "$here/scripts/lib" "$work/repo/scripts/"
}

calls() { find "$work/calls" -name 'argv.*' | wc -l | tr -d ' '; }

# run_script <endpoint> <script> [args] runs with a clean environment, so a
# credential exported in the calling shell can never reach a shim or a server.
run_script() {
  local endpoint="$1"; shift
  env -i PATH="$work/bin:$PATH" HOME="$work" TF_BIN=true LIVE_MINT_PACE=0 \
    K0RDENT_ENDPOINT="$endpoint" K0RDENT_REGION=local K0RDENT_PROJECT=p1 \
    K0RDENT_API_KEY="$DUMMY_KEY" FIXTURE_CAPTURE_CONFIRM=1 FIXTURE_DIR="$work/fixtures" \
    "$@" > "$work/out" 2>&1 < /dev/null || true
}

# --- 1. a cleartext endpoint is refused before anything is minted ----------

for script in "scripts/live.sh preflight" "scripts/capture-live-fixtures.sh"; do
  # A name that merely starts like a loopback one is not loopback, and the
  # check does no DNS, so example.invalid never has to resolve. The provider
  # reads the host as url.Parse gives it, so an uppercase LOCALHOST and an IPv6
  # zone both fail there, and they have to fail here too.
  for bad in http://example.invalid http://localhost.example.invalid http://127.0.0.1.example.invalid \
             http://LOCALHOST:1 "http://[::1%25lo0]:1" http://127.0.0.1@example.invalid:1; do
    setup
    # shellcheck disable=SC2086
    run_script "$bad" "$work/repo/"$script
    [ "$(calls)" = 0 ] || { cat "$work/out" >&2; fail "$script called curl for $bad"; }
    grep -q 'https' "$work/out" || { cat "$work/out" >&2; fail "$script did not say why it refused $bad"; }
  done

  for ok in http://127.0.0.1:1 http://localhost:1 "http://[::1]:1" https://example.invalid; do
    setup
    # shellcheck disable=SC2086
    run_script "$ok" "$work/repo/"$script
    [ "$(calls)" != 0 ] || { cat "$work/out" >&2; fail "$script refused $ok, which the provider accepts"; }
  done
done

# live-storage.sh never mints itself: the provider does, inside Terraform, and
# refuses cleartext on its own. The script checks first anyway, so a run aimed
# at the wrong endpoint stops before it builds anything or reaches LIVE_CONFIRM.
for bad in http://example.invalid http://localhost.example.invalid http://LOCALHOST:1; do
  setup
  run_script "$bad" LIVE_CONFIRM=example.invalid "$work/repo/scripts/live-storage.sh" run
  grep -q 'Use https' "$work/out" || { cat "$work/out" >&2; fail "live-storage.sh did not refuse $bad"; }
  grep -q 'built terraform-provider' "$work/out" && { cat "$work/out" >&2; fail "live-storage.sh built the provider for $bad"; }
done
for ok in http://127.0.0.1:1 https://example.invalid; do
  setup
  run_script "$ok" "$work/repo/scripts/live-storage.sh" run
  grep -q 'Confirm the deployment' "$work/out" \
    || { cat "$work/out" >&2; fail "live-storage.sh refused $ok before its LIVE_CONFIRM gate"; }
done

# --- 2. the upgrade picker does not follow a redirect -----------------------

# Two servers on 127.0.0.1. A answers every request with a 302 to B, and B
# writes down whatever reaches it. urllib follows a redirect by default and
# carries the Authorization header along, so a picker that follows it hands
# the bearer to wherever the Location header points.
cat > "$work/server.py" <<'EOF'
import http.server, os, sys
role, portfile, target = sys.argv[1], sys.argv[2], sys.argv[3]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if role == "redirect":
            self.send_response(302)
            self.send_header("Location", target + self.path)
            self.end_headers()
            return
        with open(target, "a") as f:
            f.write("%s %s\n" % (self.path, self.headers.get("Authorization")))
        body = b'{"clusterTypes":[]}'
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
with open(portfile + ".tmp", "w") as f:
    f.write(str(s.server_port))
os.rename(portfile + ".tmp", portfile)
s.serve_forever()
EOF

wait_port() {
  local _
  for _ in $(seq 1 100); do [ -s "$1" ] && { cat "$1"; return; }; sleep 0.1; done
  fail "a loopback test server did not start"
}

: > "$work/b.log"
python3 "$work/server.py" record "$work/b.port" "$work/b.log" & b_pid=$!; disown "$b_pid"
trap 'kill "$b_pid" 2>/dev/null; rm -rf "$work"' EXIT
b_port="$(wait_port "$work/b.port")"
python3 "$work/server.py" redirect "$work/a.port" "http://127.0.0.1:$b_port" & a_pid=$!; disown "$a_pid"
trap 'kill "$a_pid" "$b_pid" 2>/dev/null; rm -rf "$work"' EXIT
a_port="$(wait_port "$work/a.port")"

setup
run_script "http://127.0.0.1:$a_port" LIVE_CONFIRM="127.0.0.1:$a_port" "$work/repo/scripts/live.sh" run
[ ! -s "$work/b.log" ] || { cat "$work/b.log" >&2; fail "the upgrade picker followed a redirect and sent a request to its target"; }
grep -q "could not read the cluster type's versions" "$work/out" \
  || { cat "$work/out" >&2; fail "the upgrade picker did not fail closed on a redirect"; }
grep -q "refusing to follow a redirect" "$work/out" \
  || { cat "$work/out" >&2; fail "the upgrade picker did not say it refused a redirect"; }

# --- 3. no credential in curl's argv ----------------------------------------

# Any user on the machine can read another process's argv from ps. So the
# bearer, like the API key before it, has to reach curl some other way, and it
# still has to reach it.
for script in "scripts/live.sh preflight" "scripts/capture-live-fixtures.sh"; do
  setup
  # shellcheck disable=SC2086
  run_script https://example.invalid "$work/repo/"$script
  [ "$(calls)" -ge 2 ] || { cat "$work/out" >&2; fail "$script made no authenticated call to inspect"; }
  if grep -l -e "$DUMMY_BEARER" -e "$DUMMY_KEY" "$work"/calls/argv.*; then
    fail "$script put a credential in curl's argv"
  fi
  grep -qx "Authorization: Bearer $DUMMY_BEARER" "$work"/calls/stdin.* \
    || fail "$script did not hand curl the bearer on stdin"
done

# --- 4. the walkthroughs name no fixed path under /tmp ---------------------

# The walkthroughs build the provider binary and write the dev override that
# points Terraform at it, and the hardware one runs it with a live secret. A
# fixed directory under /tmp is one another user can create first and fill.
for doc in walkthrough.md walkthrough-hardware.md walkthrough-api.md; do
  if grep -n '/tmp/' "$here/docs/$doc"; then
    fail "docs/$doc builds or runs the provider from a fixed path in /tmp"
  fi
done

# --- 6. the docs read a client secret in rather than have it typed ----------

# A secret typed on an `export` line goes into the shell's history file. The
# walkthrough already reads it in with `read -rs`. These are the other places
# that show a reader how to supply one.
if grep -n "export K0RDENT_CLIENT_SECRET=['<.]" \
     "$here/README.md" "$here/TESTING.md" "$here/docs/index.md" "$here/docs/guides/authentication.md" \
     "$here/templates/guides/authentication.md.tmpl" "$here/examples/provider/provider.tf"; then
  fail "a document still has the client secret typed on the command line"
fi

# --- 7. the demo keeps secrets out of argv -------------------------------

# The demo's own requests to its fake carry the fake's owner token. Lifted out
# and run against a curl that answers ready, then accepts the shutdown, then
# stops answering.
OWNER="dummy-owner-token-not-a-secret"
mkdir -p "$work/demobin"
cat > "$work/demobin/curl" <<EOF
#!/usr/bin/env bash
n="\$(find "$work/calls" -name 'argv.*' | wc -l | tr -d ' ')"
printf '%s\n' "\$@" > "$work/calls/argv.\$n"
cat > "$work/calls/stdin.\$n"
case "\$n:\$*" in
  *_harness/shutdown*) printf 202 ;;
  0:*|1:*)             printf 204 ;;
  *)                   printf 000 ;;
esac
EOF
chmod +x "$work/demobin/curl"
rm -rf "$work/calls" "$work/demo" && mkdir -p "$work/calls" "$work/demo"
printf 'http://127.0.0.1:1\n' > "$work/demo/endpoint"
printf "FAKE_OWNER_TOKEN='%s'\n" "$OWNER" > "$work/demo/fake-secrets.env"
(
  PATH="$work/demobin:$PATH"
  REPO="$here"
  # shellcheck source=../lib/common.sh
  . "$here/scripts/lib/common.sh"
  # shellcheck disable=SC2034 # read by the functions lifted out with sed and eval
  PORT_FILE="$work/demo/endpoint" SECRETS_FILE="$work/demo/fake-secrets.env"
  eval "$(sed -n -e '/^owner_token() {/,/^}/p' -e '/^fake_running() {/,/^}/p' -e '/^stop_fake() {/,/^}/p' \
    "$here/scripts/demo.sh")"
  fake_running && stop_fake
) > "$work/out" 2>&1 || { cat "$work/out" >&2; fail "the demo's fake_running and stop_fake did not run against the shim"; }
grep -q '_harness/shutdown' "$work"/calls/argv.* || fail "the demo never asked its fake to shut down"
if grep -l "$OWNER" "$work"/calls/argv.*; then
  fail "scripts/demo.sh put the fake's owner token in curl's argv"
fi
grep -qx "Authorization: Bearer $OWNER" "$work"/calls/stdin.* \
  || fail "scripts/demo.sh did not hand curl the owner token on stdin"

# --- 8. capture-live-fixtures writes what it fetched, redacted -------------

# The redactor reads the response on stdin. It once took its own source from a
# heredoc on stdin too, which won, so it parsed an empty document and every
# capture failed. This curl answers a mint with a token and every read with a
# document that carries one field to redact and one to keep. The endpoint has
# a quote in its path, which is legal in a URL and has to come through into
# the manifest as data, not as code.
mkdir -p "$work/capbin"
cat > "$work/capbin/curl" <<EOF
#!/usr/bin/env bash
cat > /dev/null
case "\$*" in
  */auth/token*)    printf '{"access_token":"$DUMMY_BEARER"}' ;;
  */cluster-types*) printf '{"clusterTypes":[{"id":"ct-1","kubeconfig":"leak"}]}' ;;
  *)                printf '{"regions":[{"id":"r1","type":"regional","apiKey":"leak"}]}' ;;
esac
EOF
chmod +x "$work/capbin/curl"
setup
rm -rf "$work/fixtures"
cap_endpoint='https://example.invalid/a"b'
env -i PATH="$work/capbin:$PATH" HOME="$work" K0RDENT_ENDPOINT="$cap_endpoint" K0RDENT_REGION=r1 \
  K0RDENT_API_KEY="$DUMMY_KEY" FIXTURE_CAPTURE_CONFIRM=1 FIXTURE_DIR="$work/fixtures" \
  "$work/repo/scripts/capture-live-fixtures.sh" > "$work/out" 2>&1 < /dev/null \
  || { cat "$work/out" >&2; fail "capture-live-fixtures.sh failed against a server that answered"; }
python3 - "$work/fixtures" "$cap_endpoint" <<'PY' || fail "capture-live-fixtures.sh wrote the wrong fixtures"
import json, os, sys
d, endpoint = sys.argv[1], sys.argv[2]
regions = json.load(open(os.path.join(d, "regions.json")))
assert regions == {"regions": [{"id": "r1", "type": "regional", "apiKey": "[REDACTED]"}]}, regions
types = json.load(open(os.path.join(d, "cluster-types.json")))
assert types == {"clusterTypes": [{"id": "ct-1", "kubeconfig": "[REDACTED]"}]}, types
manifest = json.load(open(os.path.join(d, "manifest.json")))
assert manifest["endpoint"] == endpoint and manifest["region"] == "r1", manifest
PY
if grep -rl -e leak -e "$DUMMY_BEARER" -e "$DUMMY_KEY" "$work/fixtures"; then
  fail "capture-live-fixtures.sh wrote a credential into a fixture"
fi
# A fixture directory someone else planted as a symlink is refused, not
# written through.
mkdir -p "$work/elsewhere"
ln -sfn "$work/elsewhere" "$work/planted-fixtures"
if env -i PATH="$work/capbin:$PATH" HOME="$work" K0RDENT_ENDPOINT=https://example.invalid K0RDENT_REGION=r1 \
     K0RDENT_API_KEY="$DUMMY_KEY" FIXTURE_CAPTURE_CONFIRM=1 FIXTURE_DIR="$work/planted-fixtures" \
     "$work/repo/scripts/capture-live-fixtures.sh" > "$work/out" 2>&1 < /dev/null; then
  fail "capture-live-fixtures.sh wrote through a planted symlink"
fi
[ -z "$(ls "$work/elsewhere")" ] || fail "capture-live-fixtures.sh wrote into a symlink's target"
# Read-only fixtures from a real landscape are nobody else's business.
[ "$(python3 -c 'import os, stat, sys; print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode)))' "$work/fixtures")" = 0o700 ] \
  || fail "capture-live-fixtures.sh left its fixture directory readable by other users"

echo "script credentials test: PASS"
