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
#   * run refuses an endpoint that isn't on localhost before it builds or
#     creates anything.
#   * verify-release names the resources only kind ran, unless a hardware
#     record for the release's code covers them.
#   * hardware refuses localhost, a landscape it wasn't told to confirm, a
#     label that could be a host, and a run with no API version, before it
#     builds or creates anything.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "real-api-check test: FAIL - $1" >&2; exit 1; }

repo="$work/repo"
mkdir -p "$repo/scripts" "$repo/ci/real-api/lifecycle" "$repo/internal/tools/fakeapi" \
  "$repo/internal/provider" "$repo/internal/acceptance" "$repo/internal/k0rdent"
cp "$here/scripts/real-api-check.sh" "$repo/scripts/"
cp -R "$here/scripts/lib" "$repo/scripts/"
echo v1.3.0-rc.5 > "$repo/ci/real-api/api-version"
echo '[]' > "$repo/ci/fake-drift-accepted.json"
echo '# lifecycle' > "$repo/ci/real-api/lifecycle/main.tf"
echo 'package main' > "$repo/internal/tools/fakeapi/main.go"
echo 'package k0rdent' > "$repo/internal/k0rdent/client.go"
echo 'package provider' > "$repo/internal/provider/resource.go"
echo 'package provider' > "$repo/internal/provider/resource_test.go"
echo 'package acceptance' > "$repo/internal/acceptance/lifecycle_test.go"
echo 'module example.com/p' > "$repo/go.mod"
git -C "$repo" init -q
git -C "$repo" add -A

check="$repo/scripts/real-api-check.sh"

# record <api version> <step result> [gaps] writes a record for the tree as it is.
record() {
  RECORD="$repo/ci/real-api/record.json" FAKE="$("$check" hash fake)" CODE="$("$check" hash code)" \
    API="$1" STEP="$2" GAPS="${3:-}" python3 -c '
import json, os
json.dump({"fake": os.environ["FAKE"], "code": os.environ["CODE"], "api_version": os.environ["API"],
           "steps": {"fake drift": os.environ["STEP"], "live": "PASS"}, "ran": "2026-11-03T00:00:00Z",
           "landscape_gaps": os.environ["GAPS"].split()},
          open(os.environ["RECORD"], "w"))'
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

echo "real-api-check test: ok"
