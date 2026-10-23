#!/usr/bin/env bash
#
# Proves scripts/install-syft.sh refuses a download whose checksum isn't the
# pinned one, and that the release targets then stop rather than run
# goreleaser with whatever syft is on PATH. curl is a shim that writes the
# wrong bytes, so nothing here touches the network.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "install syft test: FAIL - $1" >&2; exit 1; }

mkdir -p "$work/bin"
cat > "$work/bin/curl" <<'SHIM'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do
  if [ "$1" = -o ]; then printf 'not syft\n' > "$2"; shift; fi
  shift
done
SHIM
chmod +x "$work/bin/curl"

if PATH="$work/bin:$PATH" "$here/scripts/install-syft.sh" > "$work/out" 2>&1; then
  fail "install-syft.sh accepted a download with the wrong checksum"
fi
grep -q 'has checksum .*, want ' "$work/out" || { cat "$work/out" >&2; fail "the refusal doesn't name the checksum"; }

# The release targets run install-syft.sh first. GORELEASER is a marker that
# records whether it ran.
printf '#!/usr/bin/env bash\ntouch "%s/goreleaser-ran"\n' "$work" > "$work/goreleaser"
chmod +x "$work/goreleaser"
for target in release-snapshot release; do
  if PATH="$work/bin:$PATH" make -s -C "$here" "$target" GORELEASER="$work/goreleaser" > "$work/out" 2>&1; then
    fail "make $target succeeded without a syft it could check"
  fi
  [ ! -e "$work/goreleaser-ran" ] || fail "make $target ran goreleaser after the syft install failed"
done

echo "install syft test: PASS"
