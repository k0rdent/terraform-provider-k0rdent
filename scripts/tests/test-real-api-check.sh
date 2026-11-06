#!/usr/bin/env bash
#
# Tests scripts/real-api-check.sh's record checks in a throwaway repository,
# so nothing here needs a kind stack, a credential or this checkout's record:
#
#   * verify passes on a record that names the tree's fake hash and API
#     version, and on a change to the provider or to tests alone.
#   * verify fails on a change to the fake, a new API version, a step that
#     didn't pass, and a missing record.
#   * verify-release fails on a change to the provider, and passes on a
#     change to tests alone.
#   * verify-release hashes a tree by that tree's own ci/real-api/paths, and
#     says so plainly when the lists or the hashing changed, or the file is
#     missing. It refuses an entry with a comment or a space in it, or one that
#     matches no file.
#   * the hashes of a fixed tree stay what they were for this HASHING, so a
#     change to how a hash is made can't land without a bump.
#   * run refuses a stack holding an earlier run's objects, and a stack whose
#     lists it can't read, before it builds anything.
#   * run refuses an endpoint that isn't on localhost before it builds or
#     creates anything.
#   * verify-release names the resources only kind ran, unless a hardware
#     record for the release's code covers them.
#   * hardware refuses localhost, a landscape it wasn't told to confirm, a
#     label that could be a host, and a run with no API version, before it
#     builds or creates anything.
#   * real-api-kind-setup.sh takes an API source whose HEAD is the tag's
#     commit, whatever other tag is on it, and REAL_API_FRESH=1 deletes only a
#     kind cluster the script made.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
api_pid=
trap '[ -z "$api_pid" ] || kill "$api_pid" 2>/dev/null; rm -rf "$work"' EXIT

fail() { echo "real-api-check test: FAIL - $1" >&2; exit 1; }

repo="$work/repo"
mkdir -p "$repo/scripts" "$repo/ci/real-api/lifecycle" "$repo/internal/tools/fakeapi" \
  "$repo/internal/provider" "$repo/internal/acceptance" "$repo/internal/k0rdent" \
  "$repo/internal/tools/fakedrift" "$repo/internal/repocheck" "$repo/examples/live" "$repo/examples/walkthrough/look"
cp "$here/scripts/real-api-check.sh" "$here/scripts/real-api-kind-setup.sh" "$here/scripts/live.sh" "$repo/scripts/"
cp -R "$here/scripts/lib" "$repo/scripts/"
echo v1.3.0-rc.5 > "$repo/ci/real-api/api-version"
cp "$here/ci/real-api/paths" "$repo/ci/real-api/paths"
echo '[]' > "$repo/ci/fake-drift-accepted.json"
echo '# lifecycle' > "$repo/ci/real-api/lifecycle/main.tf"
echo 'package main' > "$repo/internal/tools/fakeapi/main.go"
echo 'package main' > "$repo/internal/tools/fakedrift/main.go"
echo 'package repocheck' > "$repo/internal/repocheck/check.go"
echo '# live' > "$repo/examples/live/main.tf"
echo '# look' > "$repo/examples/walkthrough/look/main.tf"
echo 'package main' > "$repo/main.go"
: > "$repo/go.sum"
echo 'package k0rdent' > "$repo/internal/k0rdent/client.go"
echo 'package provider' > "$repo/internal/provider/resource.go"
echo 'package provider' > "$repo/internal/provider/resource_test.go"
echo 'package acceptance' > "$repo/internal/acceptance/lifecycle_test.go"
echo 'module example.com/p' > "$repo/go.mod"
git -C "$repo" init -q
git -C "$repo" add -A

check="$repo/scripts/real-api-check.sh"

# record <api version> <step result> [gaps] writes a record for the tree as it
# is. RECORD_TREE names another tree to record, and HASHING another version.
record() {
  local tree="${RECORD_TREE:-$repo}"
  RECORD="$tree/ci/real-api/record.json" FAKE="$("$check" hash fake "$tree")" CODE="$("$check" hash code "$tree")" \
    FAKE_LIST="$("$check" hash fake-list "$tree")" CODE_LIST="$("$check" hash code-list "$tree")" \
    HASHING="${HASHING:-$("$check" hash hashing)}" API="$1" STEP="$2" GAPS="${3:-}" python3 -c '
import json, os
e = os.environ
json.dump({"fake": e["FAKE"], "code": e["CODE"], "hashing": int(e["HASHING"]),
           "lists": {"fake": e["FAKE_LIST"], "code": e["CODE_LIST"]}, "api_version": e["API"],
           "steps": {"fake drift": e["STEP"], "live": "PASS"}, "ran": "2026-11-03T00:00:00Z",
           "landscape_gaps": e["GAPS"].split()},
          open(e["RECORD"], "w"))'
}

# passes <what> <command...> and fails_with <what> <stderr text> <command...>
passes() {
  local what="$1"; shift
  "$@" > "$work/out" 2>&1 || { cat "$work/out" >&2; fail "$what: exited non-zero"; }
}
fails_with() {
  local what="$1" want="$2"; shift 2
  if "$@" > "$work/out" 2>&1; then cat "$work/out" >&2; fail "$what: passed"; fi
  grep -q "$want" "$work/out" || { cat "$work/out" >&2; fail "$what: no \"$want\" in the output"; }
}

# --- a record of this tree passes both checks -------------------------------

record v1.3.0-rc.5 PASS
passes "a matching record" "$check" verify
passes "a matching record, for a release" "$check" verify-release "$repo"

# --- a record names the landscape gaps it ran with ---------------------------

record v1.3.0-rc.5 PASS "subnet-cidr no-file-store"
passes "a record with landscape gaps" "$check" verify
grep -q 'without: subnet-cidr, no-file-store' "$work/out" || { cat "$work/out" >&2; fail "verify doesn't name the record's landscape gaps"; }
record v1.3.0-rc.5 PASS

# --- a release names what only kind ran ---------------------------------------

record v1.3.0-rc.5 PASS "subnet-cidr no-file-store"
passes "a release with kind gaps and no hardware record" "$check" verify-release "$repo"
grep -q 'hardware record: none' "$work/out" || { cat "$work/out" >&2; fail "verify-release doesn't say there's no hardware record"; }
grep -q 'kind only, no hardware for: k0rdent_file_store, k0rdent_subnet$' "$work/out" \
  || { cat "$work/out" >&2; fail "verify-release doesn't name what only kind ran"; }
# hw_record <code hash> <covered...> writes a hardware record.
hw_record() {
  RECORD="$repo/ci/real-api/hardware-record.json" CODE="$1" COVERED="${*:2}" python3 -c '
import json, os
json.dump({"landscape": "lab-a", "api_version": "v1.3.0-rc.5", "api_version_source": "stated by the runner",
           "code": os.environ["CODE"], "covered": os.environ["COVERED"].split(), "ran": "2026-11-03T00:00:00Z"},
          open(os.environ["RECORD"], "w"))'
}
hw_record "$("$check" hash code)" k0rdent_kubernetes_cluster k0rdent_file_store
passes "a release with a hardware record" "$check" verify-release "$repo"
grep -q 'hardware record: API v1.3.0-rc.5 (stated by the runner) on lab-a, run 2026-11-03T00:00:00Z, covers this release' "$work/out" \
  || { cat "$work/out" >&2; fail "verify-release doesn't print the hardware record"; }
grep -q 'kind only, no hardware for: k0rdent_subnet$' "$work/out" \
  || { cat "$work/out" >&2; fail "verify-release names a resource the hardware record covers"; }
hw_record other-code k0rdent_kubernetes_cluster k0rdent_file_store k0rdent_subnet
passes "a release with a hardware record for other code" "$check" verify-release "$repo"
grep -q 'ran other code than this release' "$work/out" && grep -q 'kind only, no hardware for: k0rdent_file_store, k0rdent_subnet$' "$work/out" \
  || { cat "$work/out" >&2; fail "verify-release counts a hardware record for other code"; }
rm "$repo/ci/real-api/hardware-record.json"
record v1.3.0-rc.5 PASS

# --- a change to tests alone needs no run -----------------------------------

echo '// more' >> "$repo/internal/provider/resource_test.go"
echo '// more' >> "$repo/internal/acceptance/lifecycle_test.go"
passes "a change to tests alone" "$check" verify
passes "a change to tests alone, for a release" "$check" verify-release "$repo"

# --- a change to the provider alone needs a run for a release only ----------

echo '// more' >> "$repo/internal/provider/resource.go"
passes "a change to the provider alone" "$check" verify
fails_with "a change to the provider, for a release" "this release has code the last run did not" \
  "$check" verify-release "$repo"
record v1.3.0-rc.5 PASS

# --- a change to the fake, the client or the accepted list needs a run -------

for f in internal/tools/fakeapi/main.go internal/k0rdent/client.go ci/fake-drift-accepted.json; do
  cp "$repo/$f" "$work/saved"
  echo ' ' >> "$repo/$f"
  fails_with "a change to $f" "the fake, the API client or the real-API check changed" "$check" verify
  cp "$work/saved" "$repo/$f"
done
passes "the record again, once the fake is back" "$check" verify

# --- a release tree is hashed by its own lists ------------------------------

# A tagged release-branch tree whose ci/real-api/paths differs from this
# script's tree, as it will once main changes its lists: still covered.
tagged="$work/tagged"
cp -R "$repo" "$tagged"
echo 'code scripts/extra.sh' >> "$tagged/ci/real-api/paths"
echo 'echo extra' > "$tagged/scripts/extra.sh"
RECORD_TREE="$tagged" record v1.3.0-rc.5 PASS
passes "a release tree with lists of its own" "$check" verify-release "$tagged"
echo 'echo more' >> "$tagged/scripts/extra.sh"
fails_with "a change to a file only the release tree's list covers" "this release has code the last run did not" \
  "$check" verify-release "$tagged"
RECORD_TREE="$tagged" record v1.3.0-rc.5 PASS
echo 'code examples' >> "$tagged/ci/real-api/paths"
fails_with "a release tree whose code list changed" "the paths the code hash covers, in ci/real-api/paths, changed" \
  "$check" verify-release "$tagged"
grep -q "has code the last run did not" "$work/out" && fail "a list change is blamed on the code"
echo 'fake examples' >> "$tagged/ci/real-api/paths"
fails_with "a release tree whose fake list changed" "the paths the fake hash covers, in ci/real-api/paths, changed" \
  "$check" verify-release "$tagged"
RECORD_TREE="$tagged" HASHING=0 record v1.3.0-rc.5 PASS
fails_with "a record hashed another way" "version 0 of the hashing" "$check" verify-release "$tagged"
grep -q "has code the last run did not" "$work/out" && fail "a hashing change is blamed on the code"
rm "$tagged/ci/real-api/paths"
fails_with "a release tree with no ci/real-api/paths" "has no ci/real-api/paths" "$check" verify-release "$tagged"
rm -rf "$tagged"

# --- an entry in ci/real-api/paths has to be one path that matches a file ------

cp "$repo/ci/real-api/paths" "$work/paths.saved"
for entry in 'code scripts/live.sh # make live' 'code scripts/live.sh scripts/lib' 'code scripts/livee.sh' 'skip internal/acceptanc' 'fake'; do
  cp "$work/paths.saved" "$repo/ci/real-api/paths"
  echo "$entry" >> "$repo/ci/real-api/paths"
  case "$entry" in
    *'#'* | *' '*' '*) want="has a # or a space in its path" ;;
    fake) want="the fake line names no path" ;;
    *) want="matches no file" ;;
  esac
  fails_with "the paths entry \"$entry\"" "$want" "$check" verify-release "$repo"
  fails_with "the paths entry \"$entry\", for a pull request" "$want" "$check" verify
done
cp "$work/paths.saved" "$repo/ci/real-api/paths"
passes "the paths file again, once each entry matches" "$check" verify-release "$repo"

# --- a change to how a hash is made needs a HASHING bump ----------------------

# A fixed tree's hashes, pinned for each HASHING. A change to tree_hash or
# list_hash changes them, and fails here until HASHING is bumped and the new
# version's hashes are added.
pinned="$work/pinned"
mkdir -p "$pinned/ci/real-api" "$pinned/b"
printf 'fake a.txt\nfake ci/real-api/paths\ncode b\nskip b/x_test.go\n' > "$pinned/ci/real-api/paths"
printf 'a\n' > "$pinned/a.txt"
printf 'c\n' > "$pinned/b/c.go"
printf 't\n' > "$pinned/b/x_test.go"
git -C "$pinned" init -q
got="$("$check" hash fake "$pinned") $("$check" hash code "$pinned") $("$check" hash fake-list "$pinned") $("$check" hash code-list "$pinned")"
hashing="$("$check" hash hashing)"
case "$hashing" in
  1) want="c7f8c54ceb695a3b9641b9aece3ab668d48da527 3e0ea2d0ead96d125178acf6538783c0141da67d 7a788afd4940282591e6e7f0234ab7151234835b f99afcc51ffee5e2dc206960c5e8867943ae6261" ;;
  *) fail "HASHING is $hashing, and this test has no hashes pinned for it. Add the line: $hashing) want=\"$got\"" ;;
esac
[ "$got" = "$want" ] || fail "the hashes of a fixed tree changed, and HASHING is still $hashing: got $got, want $want. Every committed record on every branch would stop matching, blamed on the code. Bump HASHING in scripts/real-api-check.sh, and pin the new hashes here under the new version"
rm -rf "$pinned"

# --- a new API version needs a run ------------------------------------------

echo v1.3.0-rc.6 > "$repo/ci/real-api/api-version"
fails_with "a new API version" "the last run was against API v1.3.0-rc.5" "$check" verify
echo v1.3.0-rc.5 > "$repo/ci/real-api/api-version"

# --- a record with a step that didn't pass fails ----------------------------

record v1.3.0-rc.5 FAIL
fails_with "a failed step" "steps that did not pass: fake drift" "$check" verify

# --- no record fails --------------------------------------------------------

rm "$repo/ci/real-api/record.json"
fails_with "no record" "no ci/real-api/record.json" "$check" verify

# --- run refuses an endpoint that isn't on localhost ------------------------

mkdir -p "$work/bin"
cat > "$work/bin/go" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  run) printf 'Provider type name             : k0rdent\nRegistry namespace             : k0rdent\n' ;;
  *)   echo "go $* must not run before the endpoint is checked" >&2; exit 1 ;;
esac
EOF
for t in kubectl tf ssh-keygen; do
  printf '#!/usr/bin/env bash\necho "%s must not run before the endpoint is checked" >&2\nexit 1\n' "$t" > "$work/bin/$t"
done
chmod +x "$work/bin/"*
for endpoint in https://api.example.com http://localhost.example.com http://10.0.0.1:14000; do
  fails_with "run against $endpoint" "runs against a local kind stack only" \
    env -i PATH="$work/bin:$PATH" HOME="$work" TF_BIN="$work/bin/tf" K0RDENT_ENDPOINT="$endpoint" \
    "$check" run
done

# --- hardware refuses before it builds or creates anything ---------------------

# hw_env <endpoint> [VAR=value...] runs hardware with everything else it needs set.
hw_env() {
  local endpoint="$1"; shift
  env -i PATH="$work/bin:$PATH" HOME="$work" TF_BIN="$work/bin/tf" K0RDENT_ENDPOINT="$endpoint" \
    K0RDENT_REGION=r K0RDENT_PROJECT=p K0RDENT_CLIENT_ID=id K0RDENT_CLIENT_SECRET=secret \
    REAL_API_CLUSTER_TYPE=ct REAL_API_VERSION=v1.3.0-rc.5 "$@" "$check" hardware
}
for endpoint in http://localhost:14000 http://127.0.0.1:14000 https://localhost; do
  fails_with "hardware against $endpoint" "on this machine. Hardware mode runs against a landscape with hardware" \
    hw_env "$endpoint" REAL_API_CONFIRM="${endpoint#*://}"
done
fails_with "hardware against plain http" "Use https" hw_env http://api.example.com REAL_API_CONFIRM=api.example.com
fails_with "hardware with no confirm" "REAL_API_CONFIRM=api.example.com make real-api-check-hardware" \
  hw_env https://api.example.com
fails_with "hardware with another host confirmed" "They have to match" \
  hw_env https://api.example.com REAL_API_CONFIRM=api.example.org
fails_with "hardware with an unknown switch off" "REAL_API_BUILD_OFF names webhook" \
  hw_env https://api.example.com REAL_API_CONFIRM=api.example.com REAL_API_BUILD_OFF=webhook
fails_with "hardware with a host as its label" "never a host name" \
  hw_env https://api.example.com REAL_API_CONFIRM=api.example.com REAL_API_LANDSCAPE=api.example.com
fails_with "hardware with no API version" "no API version" \
  hw_env https://api.example.com REAL_API_CONFIRM=api.example.com REAL_API_VERSION=
printf '#!/usr/bin/env bash\n[ "$1" = version ] && echo "Terraform v1.16.3" && exit 0\necho "tf $* must not run" >&2; exit 1\n' > "$work/bin/tf-1.16.3"
chmod +x "$work/bin/tf-1.16.3"
fails_with "hardware under Terraform 1.16.3" "can lose the whole state when a run is interrupted" \
  hw_env https://api.example.com REAL_API_CONFIRM=api.example.com TF_BIN="$work/bin/tf-1.16.3"
[ ! -d "$repo/.real-api" ] || fail "a refused hardware run made $repo/.real-api"

# --- run refuses a stack it can't show is clean -------------------------------

# A stand-in for the stack's gateway. Every list is empty unless API_ANSWERS
# names a path ending and the status and body to answer it with.
cat > "$work/api.py" <<'PY'
import http.server, os
answers = dict(a.split("=", 1) for a in os.environ.get("API_ANSWERS", "").split())
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, body):
        b = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.send(200, '{"access_token":"t"}')
    def do_GET(self):
        for end, answer in answers.items():
            if self.path.endswith(end):
                code, _, body = answer.partition(":")
                return self.send(int(code), body)
        if self.path.endswith("/organizations"):
            return self.send(200, '{"organizations":[{"id":"o"}]}')
        if self.path.endswith("/organizations/o/projects"):
            return self.send(200, '{"projects":[{"id":"p"}]}')
        self.send(200, '{"items":[]}')
s = http.server.HTTPServer(("127.0.0.1", 0), H)
print(s.server_address[1], flush=True)
s.serve_forever()
PY
mkdir -p "$work/stackbin"
cp "$work/bin/go" "$work/bin/tf" "$work/bin/ssh-keygen" "$work/stackbin/"
printf '#!/usr/bin/env bash\necho ghcr.io/example/k0r-compute:v1.3.0-rc.5\n' > "$work/stackbin/kubectl"
chmod +x "$work/stackbin/kubectl"
record v1.3.0-rc.5 PASS

# leftovers <API_ANSWERS> runs run against the stand-in, which answers so.
leftovers() {
  local port
  API_ANSWERS="$1" python3 "$work/api.py" > "$work/api.port" &
  api_pid=$!
  for _ in $(seq 1 50); do [ -s "$work/api.port" ] && break; sleep 0.1; done
  port="$(cat "$work/api.port")"
  env -i PATH="$work/stackbin:$PATH" HOME="$work" TF_BIN="$work/stackbin/tf" K0RDENT_ENDPOINT="http://127.0.0.1:$port" \
    K0RDENT_REGION=r K0RDENT_PROJECT=p K0RDENT_CLIENT_ID=id K0RDENT_CLIENT_SECRET=secret \
    REAL_API_ORG_CLIENT_ID=oid REAL_API_ORG_CLIENT_SECRET=osecret REAL_API_CLUSTER_TYPE=ct \
    "$check" run > "$work/out" 2>&1 || true
  kill "$api_pid" 2>/dev/null; wait "$api_pid" 2>/dev/null || true
  api_pid=
  : > "$work/api.port"
  return 0
}
clean_stack() {
  grep -q "the stack holds nothing from an earlier run" "$work/out" || { cat "$work/out" >&2; fail "$1: not taken as a clean stack"; }
}
refused() {
  grep -q "$2" "$work/out" || { cat "$work/out" >&2; fail "$1: no \"$2\" in the output"; }
  grep -q "the stack holds nothing from an earlier run" "$work/out" && { cat "$work/out" >&2; fail "$1: taken as a clean stack"; }
  return 0
}
leftovers ""
clean_stack "a stack with nothing on it"
leftovers 'compute/clusters=200:{"clusters":[{"id":"c1"}]}'
refused "a stack with a cluster on it" "the stack holds what an earlier run left: clusters c1"
leftovers 'organizations/o/projects=200:{"projects":[{"id":"p"},{"id":"p2"}]}'
refused "a stack with another project" "the stack holds what an earlier run left: project p2"
for answer in "networking/vpcs=403:{}" "notifications/webhooks=500:{}" "storage/file-stores=404:{}" \
    "networking/networks=200:nope" "/organizations=403:{}"; do
  case "$answer" in *=200:*) want="answered 200 with no JSON object" ;; *) want="answered ${answer#*=}" want="${want%%:*}, not 200" ;; esac
  leftovers "$answer"
  refused "a stack answering $answer" "$want"
done

# --- the stack setup takes the tagged source and deletes only its own cluster --

src="$work/src"
mkdir -p "$src/chart" "$src/env"
cat > "$src/Makefile" <<'MK'
HELM_UMBRELLA = chart
KIND_VALUES ?= $(HELM_UMBRELLA)/values-kind.yaml
kind-install:
	@true
helm-dep-build:
	@echo "the test's source stops here" >&2; exit 1
MK
echo 'name: chart' > "$src/chart/Chart.yaml"
echo '{}' > "$src/chart/values-kind.yaml"
printf 'users:\n  - id: usr-kind-admin\n    email: admin@kind.test\n    principalType: operator\npolicies:\n  - role: r\n' \
  > "$src/env/manifest.yaml"
git -C "$src" init -q
git -C "$src" add -A
git -C "$src" -c user.name=t -c user.email=t@example.com commit -q -m tagged
git -C "$src" tag v1.3.0-rc.5
# Another tag on the same commit, which git describe prefers.
git -C "$src" -c user.name=t -c user.email=t@example.com tag -a -m other aa-other
mkdir -p "$work/setupbin"
cp "$work/bin/go" "$work/setupbin/"
for t in helm docker; do
  printf '#!/usr/bin/env bash\necho "%s must not run" >&2\nexit 1\n' "$t" > "$work/setupbin/$t"
done
cat > "$work/setupbin/kind" <<'SH'
#!/usr/bin/env bash
case "$*" in
  "get clusters") [ -z "${STUB_CLUSTER:-}" ] || echo "$STUB_CLUSTER" ;;
  "delete cluster --name "*) echo "$*" >> "$STUB_LOG" ;;
  *) echo "kind $* must not run" >&2; exit 1 ;;
esac
SH
cat > "$work/setupbin/kubectl" <<'SH'
#!/usr/bin/env bash
case "$*" in
  *"get configmap real-api-kind-setup -o jsonpath={.data.made-by}") printf '%s' "${STUB_MADE:-}" ;;
  *"get configmap real-api-kind-setup -o jsonpath={.data.reused}") printf '%s' "${STUB_REUSED:-}" ;;
  *"get deploy"*) [ -z "${STUB_COMPUTE:-}" ] || echo deployment.apps/compute ;;
  *) echo "kubectl $* must not run" >&2; exit 1 ;;
esac
SH
chmod +x "$work/setupbin/"*

# setup [VAR=value...] runs the setup script against the test's source.
setup() {
  : > "$work/kind.log"
  env -i PATH="$work/setupbin:$PATH" HOME="$work" REAL_API_SOURCE="$src" STUB_LOG="$work/kind.log" "$@" \
    "$repo/scripts/real-api-kind-setup.sh" > "$work/out" 2>&1 && { cat "$work/out" >&2; fail "setup $*: passed"; }
  return 0
}
reached_install() {
  grep -q "API source at v1.3.0-rc.5" "$work/out" && grep -q "helm-deps failed" "$work/out" \
    || { cat "$work/out" >&2; fail "$1: didn't reach the install"; }
}
setup
reached_install "a source at the tag, with another tag on its commit"
git -C "$src" -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m later
setup
grep -q "is at [0-9a-f]*, not at v1.3.0-rc.5" "$work/out" || { cat "$work/out" >&2; fail "a source past the tag: not refused"; }
git -C "$src" checkout -q v1.3.0-rc.5
for stub in "STUB_COMPUTE=1" "STUB_MADE=x STUB_REUSED=true"; do
  # shellcheck disable=SC2086 # the stub's variables, split on purpose
  setup STUB_CLUSTER=k0rdent-rc5 REAL_API_FRESH=1 $stub
  grep -q "REAL_API_FRESH=1 deletes only a kind cluster this script made" "$work/out" && [ ! -s "$work/kind.log" ] \
    || { cat "$work/out" "$work/kind.log" >&2; fail "REAL_API_FRESH=1 with a cluster it didn't make ($stub): not refused"; }
done
for stub in "STUB_MADE=x" "STUB_MADE=x STUB_REUSED=false"; do
  # shellcheck disable=SC2086 # the stub's variables, split on purpose
  setup STUB_CLUSTER=k0rdent-rc5 REAL_API_FRESH=1 $stub
  reached_install "REAL_API_FRESH=1 with its own cluster ($stub)"
  grep -qx "delete cluster --name k0rdent-rc5" "$work/kind.log" \
    || { cat "$work/out" "$work/kind.log" >&2; fail "REAL_API_FRESH=1 with its own cluster ($stub): not deleted"; }
done
setup STUB_CLUSTER=k0rdent-rc5
grep -q "this script didn't make it, and it runs no platform API" "$work/out" \
  || { cat "$work/out" >&2; fail "a cluster it didn't make, running no API: not refused"; }

echo "real-api-check test: ok"
