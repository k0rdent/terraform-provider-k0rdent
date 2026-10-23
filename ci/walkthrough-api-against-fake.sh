#!/usr/bin/env bash
#
# Runs docs/walkthrough-api.md against the fake, from the `sh` blocks the page
# shows, so the page fails CI when it stops working rather than when a reader
# finds out on real hardware.
#
# It runs the blocks twice, in bash and, when it's installed, zsh: once in the
# page's order, and once with the cancel after step 3 and step 4 waiting it
# out, in place of step 5. Only the values a reader fills in at step 1 are
# replaced. kubectl is a stand-in, since the fake isn't a Kubernetes API. curl
# is wrapped, and no run may hand it the client secret or the token as an
# argument.
#
# Requires: go, python3, curl and jq.

set -euo pipefail

cd "$(dirname "$0")/.."
REPO="$PWD"
# shellcheck source=../scripts/lib/common.sh
. "$REPO/scripts/lib/common.sh"

WORK="$(private_tmpdir)" || exit 1
FAKE_PID=""
cleanup() {
  [ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { printf '\nwalkthrough-api: FAIL - %s\n' "$1" >&2; exit 1; }

go build -o "$WORK/fakeapi" ./internal/tools/fakeapi
start_loopback_server "$WORK/fakeapi" -secrets-file "$WORK/fake-secrets.env" "$WORK/fake.url" "$WORK/fake.log" \
  || { cat "$WORK/fake.log"; fail "the fake did not start"; }
FAKE_PID="$SERVER_PID"
# shellcheck source=/dev/null # written by internal/tools/fakeapi
. "$WORK/fake-secrets.env"

mkdir -p "$WORK/bin"
REAL_CURL="$(command -v curl)"
cat > "$WORK/bin/curl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$WORK/curl-argv.log"
exec "$REAL_CURL" "\$@"
EOF
cat > "$WORK/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
echo "kubectl $*"
EOF
chmod +x "$WORK/bin/curl" "$WORK/bin/kubectl"

# The page's blocks, as one script per path. The page's order is setup, check,
# create, wait, kubeconfig, delete, gone, clean up, then the cancel.
python3 - "$REPO/docs/walkthrough-api.md" "$SERVER_URL" "$WORK" <<'PY'
import re, sys
doc, url, work = sys.argv[1:]
blocks = re.findall(r"^```sh\n(.*?)^```", open(doc).read(), re.S | re.M)
if len(blocks) != 9:
    sys.exit("the page's sh blocks changed shape: %d of them. Update this test with it." % len(blocks))
setup = blocks[0]
for old, new in [
    ("export K0RDENT_ENDPOINT=https://<the gateway's host>", "export K0RDENT_ENDPOINT=" + url),
    ("export K0RDENT_CLIENT_ID=<the service account's client id>", 'export K0RDENT_CLIENT_ID="$FAKE_CLIENT_ID"'),
    ("<region id>", "us-texas-1"), ("<a project id>", "demo"), ("<a cluster type id>", "gpu-training"),
]:
    if old not in setup:
        sys.exit("step 1 no longer says %r. Update this test with it." % old)
    setup = setup.replace(old, new)
read = re.compile(r"printf 'client secret: ' >&2\nread -rs K0RDENT_CLIENT_SECRET[^\n]*\n")
if not read.search(setup):
    sys.exit("step 1 no longer reads the secret in with read -rs")
setup = read.sub('export K0RDENT_CLIENT_SECRET="$FAKE_CLIENT_SECRET"\n', setup)
paths = {"lifecycle": [setup] + blocks[1:8], "cancel": [setup, blocks[1], blocks[2], blocks[8], blocks[3], blocks[5], blocks[6], blocks[7]]}
for name, parts in paths.items():
    open("%s/%s.sh" % (work, name), "w").write("\n".join(parts).replace("sleep 30", "sleep 1"))
PY

# run <shell> <path> <what the output has to say, in order>...
run() {
  local shell="$1" path="$2" out="$WORK/$1-$2.out" at=0 want line limit=()
  shift 2
  # A wait loop that never ends would hang the job, so bound it where possible.
  command -v timeout >/dev/null && limit=(timeout 120)
  step "$shell: $path"
  ( cd "$WORK" && PATH="$WORK/bin:$PATH" FAKE_CLIENT_ID="$FAKE_CLIENT_ID" FAKE_CLIENT_SECRET="$FAKE_CLIENT_SECRET" \
      "${limit[@]}" "$shell" "$WORK/$path.sh" ) > "$out" 2>&1 || { cat "$out"; fail "$shell: $path exited non-zero"; }
  # Each wanted line has to come after the one before it.
  for want in "$@"; do
    line="$(tail -n +"$((at + 1))" "$out" | grep -n -m1 -F -- "$want" | cut -d: -f1)" \
      || { cat "$out"; fail "$shell: $path never said \"$want\" after line $at"; }
    at=$((at + line))
  done
}

for shell in bash zsh; do
  command -v "$shell" >/dev/null || { [ "$shell" = bash ] && fail "bash is not installed"; continue; }
  run "$shell" lifecycle "token ok" '"status": "available"' "HTTP 201" "active" "HTTP 204" "gone"
  run "$shell" cancel "token ok" "HTTP 201" "cancelling" "HTTP 202" "failed" '"stateReason":"CANCELLED"' "HTTP 204" "gone"
done

if grep -n -F -e "$FAKE_CLIENT_SECRET" -e "fake-access-token" "$WORK/curl-argv.log"; then
  fail "the page hands curl the client secret or the token as an argument"
fi

printf '\ndocs/walkthrough-api.md works against the fake, both ways.\n'
