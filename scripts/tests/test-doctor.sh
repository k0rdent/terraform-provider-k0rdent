#!/usr/bin/env bash
# Doctor must not green-light golangci-lint v1, or a v2 built with a Go older
# than go.mod. Install hints on Linux must not be Homebrew-only.
set -euo pipefail

cd "$(dirname "$0")/../.."
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() {
  echo "doctor-script-test: $*" >&2
  exit 1
}

write_lint() {
  local path="$1" stdout="$2"
  cat > "$path" <<EOF
#!/bin/sh
if [ "\$1" = "version" ]; then
  printf '%s\n' '$stdout'
  exit 0
fi
echo "unexpected: \$*" >&2
exit 1
EOF
  chmod +x "$path"
}

run_doctor() {
  local bindir="$1" out="$2"
  # Keep the rest of PATH so go/python/make/terraform stay visible; only the
  # linter (and, below, missing-tool probes) is stubbed.
  PATH="$bindir:$PATH" ./scripts/doctor.sh >"$out" 2>&1 || true
}

# --- v1 must fail Tier 1 ----------------------------------------------------
mkdir -p "$tmp/v1"
write_lint "$tmp/v1/golangci-lint" \
  "golangci-lint has version 1.64.8 built with go1.23.6 from abc on 2024-01-01"
run_doctor "$tmp/v1" "$tmp/v1.out"
grep -q 'golangci-lint 1.64.8 is too old; need v2' "$tmp/v1.out" \
  || fail "v1 was not rejected; output:"$'\n'"$(cat "$tmp/v1.out")"
grep -q 'make lint-install' "$tmp/v1.out" \
  || fail "v1 rejection did not point at make lint-install"
if grep -q 'Tier 1 is ready' "$tmp/v1.out"; then
  fail "doctor reported Tier 1 ready with golangci-lint v1"
fi
grep -q 'Tier 1 cannot run' "$tmp/v1.out" \
  || fail "v1 rejection did not fail Tier 1"

# --- v2 built with Go 1.25 must fail ----------------------------------------
mkdir -p "$tmp/oldgo"
write_lint "$tmp/oldgo/golangci-lint" \
  "golangci-lint has version 2.5.0 built with go1.25.0 from abc on 2025-01-01"
run_doctor "$tmp/oldgo" "$tmp/oldgo.out"
grep -q 'golangci-lint 2.5.0 was built with go1.25.0' "$tmp/oldgo.out" \
  || fail "old-Go v2 was not rejected; output:"$'\n'"$(cat "$tmp/oldgo.out")"
if grep -q 'Tier 1 is ready' "$tmp/oldgo.out"; then
  fail "doctor reported Tier 1 ready with golangci-lint built on go1.25"
fi

# --- v2 built with the go.mod Go must pass the linter check -----------------
mkdir -p "$tmp/ok"
write_lint "$tmp/ok/golangci-lint" \
  "golangci-lint has version 2.13.2 built with go1.26.10 from abc on 2025-01-01"
run_doctor "$tmp/ok" "$tmp/ok.out"
grep -qE 'ok +golangci-lint 2.13.2 \(built with go1.26.10\)' "$tmp/ok.out" \
  || fail "compatible v2 was not accepted; output:"$'\n'"$(cat "$tmp/ok.out")"
if grep -qE 'MISS +golangci-lint' "$tmp/ok.out"; then
  fail "compatible v2 was marked MISS"
fi

# --- An older patch of the go.mod minor is too old ---------------------------
# go.mod declares a patch release, so the floor compares the patch too: 1.26.9
# sorts below 1.26.10, which a major.minor compare would have let through.
mkdir -p "$tmp/oldpatch"
write_lint "$tmp/oldpatch/golangci-lint" \
  "golangci-lint has version 2.13.2 built with go1.26.9 from abc on 2025-01-01"
run_doctor "$tmp/oldpatch" "$tmp/oldpatch.out"
grep -q 'was built with go1.26.9; need go1.26.10+' "$tmp/oldpatch.out" \
  || fail "a linter built with go1.26.9 was not rejected; output:"$'\n'"$(cat "$tmp/oldpatch.out")"

# --- Makefile pins GOTOOLCHAIN to go.mod ------------------------------------
grep -q 'GOTOOLCHAIN=\$(GOLANGCI_LINT_GOTOOLCHAIN)' Makefile \
  || fail "lint-install does not set GOTOOLCHAIN"
grep -q 'MODULE_GO_VERSION' Makefile \
  || fail "Makefile does not read the module Go version for lint-install"

# --- The dev override check reads every shape Terraform accepts ------------
# One line, several lines, a full registry address, an empty file, and a
# directory with no binary in it.
mkdir -p "$tmp/rc/bin"
printf '#!/bin/sh\n' > "$tmp/rc/bin/terraform-provider-k0rdent"
chmod +x "$tmp/rc/bin/terraform-provider-k0rdent"

override_case() {
  local name="$1" expect="$2" body="$3"
  printf '%s\n' "$body" > "$tmp/rc/$name.rc"
  TF_CLI_CONFIG_FILE="$tmp/rc/$name.rc" PATH="$tmp/ok:$PATH" ./scripts/doctor.sh \
    >"$tmp/rc/$name.out" 2>&1 || true
  grep -q "$expect" "$tmp/rc/$name.out" \
    || fail "override case $name did not report '$expect'; output:"$'\n'"$(cat "$tmp/rc/$name.out")"
}

override_case one-line 'dev override resolves' \
  "provider_installation { dev_overrides { \"k0rdent/k0rdent\" = \"$tmp/rc/bin\" } direct {} }"
override_case multi-line 'dev override resolves' \
  "provider_installation {
  dev_overrides {
    \"k0rdent/k0rdent\" = \"$tmp/rc/bin\"
  }
  direct {}
}"
override_case registry-host 'dev override resolves' \
  "provider_installation {
  dev_overrides {
    \"registry.terraform.io/k0rdent/k0rdent\" = \"$tmp/rc/bin\"
  }
}"
override_case empty 'has no dev override in it' ''
override_case no-binary 'names a directory with no provider binary' \
  "provider_installation {
  dev_overrides {
    \"k0rdent/k0rdent\" = \"$tmp/rc/nowhere\"
  }
}"
TF_CLI_CONFIG_FILE="$tmp/rc/absent.rc" PATH="$tmp/ok:$PATH" ./scripts/doctor.sh \
  >"$tmp/rc/absent.out" 2>&1 || true
grep -q 'points at a file that is not there' "$tmp/rc/absent.out" \
  || fail "a missing TF_CLI_CONFIG_FILE was not reported"

# --- Linux hints are not Homebrew-only --------------------------------------
# The hint is only printed for a tool that is missing, and a GitHub runner has
# kind installed, so on CI a check that waited for a real kind miss asserted
# nothing. Instead, on any host, uname says Linux and kind is hidden from PATH.
mkdir -p "$tmp/linux"
printf '#!/bin/sh\necho Linux\n' > "$tmp/linux/uname"
chmod +x "$tmp/linux/uname"

# PATH with every kind removed. A directory holding one is replaced by a
# mirror of everything else in it, so go, terraform and python stay visible.
without_kind=""
i=0
IFS=: read -ra path_dirs <<<"$PATH"
for dir in "${path_dirs[@]}"; do
  if [ -e "$dir/kind" ]; then
    mirror="$tmp/nokind/$i"
    mkdir -p "$mirror"
    for entry in "$dir"/*; do
      [ "$(basename "$entry")" = kind ] || ln -s "$entry" "$mirror/"
    done
    dir="$mirror"
  fi
  without_kind="${without_kind:+$without_kind:}$dir"
  i=$((i + 1))
done

PATH="$tmp/linux:$tmp/ok:$without_kind" ./scripts/doctor.sh >"$tmp/linux.out" 2>&1 || true
grep -q 'kind is not on PATH' "$tmp/linux.out" \
  || fail "kind was hidden from PATH and doctor did not report it; output:"$'\n'"$(cat "$tmp/linux.out")"
grep -q 'kind.sigs.k8s.io' "$tmp/linux.out" \
  || fail "Linux kind miss did not give a non-brew hint"
if grep -q 'brew install' "$tmp/linux.out"; then
  fail "Linux doctor printed a Homebrew hint"
fi

echo "doctor-script-test: ok"
