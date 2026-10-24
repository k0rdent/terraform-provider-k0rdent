#!/usr/bin/env bash
#
# Proves scripts/install-syft.sh refuses a download whose checksum isn't the
# pinned one, that the release targets then stop before fetching or running
# goreleaser, and that scripts/run-goreleaser.sh runs the goreleaser it
# fetched, never one already on PATH. curl and go are shims, so nothing here
# touches the network.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "release tools test: FAIL - $1" >&2; exit 1; }

mkdir -p "$work/bin"
cat > "$work/bin/curl" <<'SHIM'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then printf 'not syft\n' > "$2"; shift; fi
  shift
done
SHIM
chmod +x "$work/bin/curl"

mkdir -p "$work/tmp"
if TMPDIR="$work/tmp" PATH="$work/bin:$PATH" "$here/scripts/install-syft.sh" > "$work/out" 2>&1; then
  fail "install-syft.sh accepted a download with the wrong checksum"
fi
grep -q 'has checksum .*, want ' "$work/out" || { cat "$work/out" >&2; fail "the refusal doesn't name the checksum"; }
[ -z "$(ls -A "$work/tmp")" ] || fail "install-syft.sh left its download behind after refusing it"

# go is a stand-in too. Only `go install` does anything: it writes a
# goreleaser into GOBIN that records it ran. A stale goreleaser on PATH records
# if it ran instead.
export RELEASE_TOOLS_WORK="$work"
cat > "$work/bin/go" <<'SHIM'
#!/usr/bin/env bash
[ "$1" = install ] || exit 0
touch "$RELEASE_TOOLS_WORK/go-ran"
cat > "$GOBIN/goreleaser" <<'TOOL'
#!/usr/bin/env bash
echo "$@" > "$RELEASE_TOOLS_WORK/fresh-ran"
TOOL
chmod +x "$GOBIN/goreleaser"
SHIM
chmod +x "$work/bin/go"
printf '#!/usr/bin/env bash\ntouch "%s/stale-ran"\n' "$work" > "$work/bin/goreleaser"
chmod +x "$work/bin/goreleaser"

# The release targets fetch syft first, so when it can't be checked neither
# goreleaser is fetched or run.
for target in release-snapshot release; do
  if PATH="$work/bin:$PATH" make -s -C "$here" "$target" > "$work/out" 2>&1; then
    fail "make $target succeeded without a syft it could check"
  fi
  [ ! -e "$work/go-ran" ] || fail "make $target fetched goreleaser after the syft install failed"
  [ ! -e "$work/stale-ran" ] || fail "make $target ran the goreleaser already on PATH"
done

# With syft fetched, here a stand-in that makes the directory, the goreleaser
# fetched for this run is the one that runs, and its directory goes after.
cat > "$work/fake-syft-installer" <<'SHIM'
#!/usr/bin/env bash
mkdir -p "$RELEASE_TOOLS_WORK/tools" && echo "$RELEASE_TOOLS_WORK/tools"
SHIM
chmod +x "$work/fake-syft-installer"
PATH="$work/bin:$PATH" SYFT_INSTALLER="$work/fake-syft-installer" GORELEASER_VERSION=v0.0.0 \
  "$here/scripts/run-goreleaser.sh" release --clean > "$work/out" 2>&1 || { cat "$work/out" >&2; fail "run-goreleaser.sh failed"; }
[ "$(cat "$work/fresh-ran" 2>/dev/null)" = "release --clean" ] || fail "the goreleaser fetched for this run didn't run with the arguments given"
[ ! -e "$work/stale-ran" ] || fail "run-goreleaser.sh ran the goreleaser already on PATH"
[ ! -e "$work/tools" ] || fail "run-goreleaser.sh left its tools directory behind"

echo "release tools test: PASS"
