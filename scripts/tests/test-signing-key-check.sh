#!/usr/bin/env bash
# The signing key check has to pass the key a release can use, and refuse each
# way a rotation or a secret goes wrong: a parent in GitHub, a subkey near
# expiry, a secret that doesn't match the committed key, and an old subkey left
# in GitHub after a rotation. A check that passes everything looks the same as
# one that works until the day it's needed. Every key here is a throwaway.
set -euo pipefail

cd "$(dirname "$0")/../.."
check="$PWD/scripts/check-signing-key.sh"
work="$(mktemp -d)"
cleanup() {
  for home in "$work"/*/; do gpgconf --homedir "$home" --kill all >/dev/null 2>&1 || true; done
  rm -rf "$work"
}
trap cleanup EXIT

fail() { echo "signing-key-check-test: FAIL - $*" >&2; exit 1; }

day=86400
pass=throwaway-passphrase
printf '%s' "$pass" >"$work/passphrase"

# make_key NAME [PARENT_ALGO]: a parent that only certifies, 10 years, and a
# signing subkey, 2 years, the shape the release key has. Prints nothing;
# leaves the keyring in $work/NAME.
make_key() {
  local home="$work/$1" algo="${2:-rsa2048}"
  install -d -m 700 "$home"
  gpg --homedir "$home" --batch --pinentry-mode loopback --passphrase "$pass" \
    --quick-generate-key "throwaway $1 <$1@example.invalid>" "$algo" cert 10y 2>/dev/null
  gpg --homedir "$home" --batch --pinentry-mode loopback --passphrase "$pass" \
    --quick-add-key "$(fpr_of "$1")" rsa2048 sign 2y 2>/dev/null
}
fpr_of() { gpg --homedir "$work/$1" --with-colons --list-keys 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }'; }
public_of() { gpg --homedir "$work/$1" --armor --export "$(fpr_of "$1")" 2>/dev/null; }
subkeys_of() { gpg --homedir "$work/$1" --batch --pinentry-mode loopback --passphrase "$pass" --armor --export-secret-subkeys "$(fpr_of "$1")"; }
full_of() { gpg --homedir "$work/$1" --batch --pinentry-mode loopback --passphrase "$pass" --armor --export-secret-keys "$(fpr_of "$1")"; }
created_of() { gpg --homedir "$work/$1" --with-colons --list-keys 2>/dev/null | awk -F: '$1 == "sub" { print $6 }' | tail -n 1; }

# run_secret OUT PUBLIC_FILE SECRET FINGERPRINT [PASSPHRASE_FILE]: the secret
# mode, in a fresh keyring the way the workflow gives it one.
run_secret() {
  local out="$1" keyfile="$2" secret="$3" fpr="$4" passfile="${5:-$work/passphrase}"
  local home
  home="$(mktemp -d "$work/ci.XXXXXX")"
  chmod 700 "$home"
  GNUPGHOME="$home" GPG_PRIVATE_KEY="$secret" GPG_KEY_FINGERPRINT="$fpr" GPG_PASSPHRASE_FILE="$passfile" \
    "$check" secret "$keyfile" >"$out" 2>&1
}
expect_fail() {
  local want="$1"
  shift
  if "$@"; then cat "$work/out" >&2; fail "accepted, wanted: $want"; fi
  grep -Fq "$want" "$work/out" || { cat "$work/out" >&2; fail "message did not mention: $want"; }
}

make_key good
public_of good >"$work/good.asc"
good_fpr="$(fpr_of good)"
created="$(created_of good)"

# The key a release can use passes both ways.
"$check" public "$work/good.asc" >"$work/out" 2>&1 || { cat "$work/out" >&2; fail "a fresh key was refused"; }
grep -Fq "rotate by" "$work/out" || fail "the public check didn't say when to rotate"
run_secret "$work/out" "$work/good.asc" "$(subkeys_of good)" "$good_fpr" ||
  { cat "$work/out" >&2; fail "a subkey-only secret was refused"; }
grep -Fq "holds no parent" "$work/out" || fail "the secret check didn't say what it proved"

# A fingerprint pasted in lowercase with spaces still names the key.
spaced="$(printf '%s' "$good_fpr" | tr '[:upper:]' '[:lower:]' | sed 's/..../& /g')"
run_secret "$work/out" "$work/good.asc" "$(subkeys_of good)" "$spaced" ||
  { cat "$work/out" >&2; fail "a fingerprint written with spaces was refused"; }

# The whole parent in GitHub.
expect_fail "holds the parent's secret part" \
  run_secret "$work/out" "$work/good.asc" "$(full_of good)" "$good_fpr"

# Past the rotation date: a warning, not a failure.
SIGNING_KEY_NOW=$((created + 400 * day)) "$check" public "$work/good.asc" >"$work/out" 2>&1 ||
  { cat "$work/out" >&2; fail "a key past its rotation date failed instead of warning"; }
grep -Fq "::warning title=Signing key due to rotate::" "$work/out" || fail "a key past its rotation date didn't warn"

# Under 90 days to expiry.
SIGNING_KEY_NOW=$((created + 700 * day)) "$check" public "$work/good.asc" >"$work/out" 2>&1 &&
  { cat "$work/out" >&2; fail "a subkey 30 days from expiry passed"; }
grep -Fq "Signing key near expiry" "$work/out" || fail "a subkey near expiry didn't say so"

# Past expiry there's no subkey left that can sign.
SIGNING_KEY_NOW=$((created + 800 * day)) "$check" public "$work/good.asc" >"$work/out" 2>&1 &&
  { cat "$work/out" >&2; fail "an expired subkey passed"; }
grep -Fq "no subkey that can still sign" "$work/out" || fail "an expired subkey didn't say so"

# A secret for one key against the committed key of another.
make_key other
public_of other >"$work/other.asc"
expect_fail "Fingerprint doesn't match" \
  run_secret "$work/out" "$work/other.asc" "$(subkeys_of good)" "$good_fpr"
# The fingerprint right, the key in the secret someone else's.
expect_fail "holds no key" \
  run_secret "$work/out" "$work/good.asc" "$(subkeys_of other)" "$good_fpr"

# A wrong passphrase.
printf 'wrong' >"$work/wrong"
expect_fail "Signing failed" \
  run_secret "$work/out" "$work/good.asc" "$(subkeys_of good)" "$good_fpr" "$work/wrong"

# A rotation that reached only one side. The new subkey is made a second
# later, so it's the newest.
old_secret="$(subkeys_of good)"
old_public="$work/good.asc"
sleep 1
gpg --homedir "$work/good" --batch --pinentry-mode loopback --passphrase "$pass" \
  --quick-add-key "$good_fpr" rsa2048 sign 2y 2>/dev/null
public_of good >"$work/rotated.asc"
expect_fail "GitHub has an old subkey" \
  run_secret "$work/out" "$work/rotated.asc" "$old_secret" "$good_fpr"
expect_fail "Subkey not committed" \
  run_secret "$work/out" "$old_public" "$(subkeys_of good)" "$good_fpr"
run_secret "$work/out" "$work/rotated.asc" "$(subkeys_of good)" "$good_fpr" ||
  { cat "$work/out" >&2; fail "a rotation that reached both sides was refused"; }

# A missing secret.
expect_fail "GPG_PRIVATE_KEY is not set" \
  run_secret "$work/out" "$work/good.asc" "" "$good_fpr"

# An ECC parent, which gpg offers by default and the registry rejects.
make_key ecc ed25519
public_of ecc >"$work/ecc.asc"
"$check" public "$work/ecc.asc" >"$work/out" 2>&1 && { cat "$work/out" >&2; fail "an ECC key passed"; }
grep -Fq "Unsupported signing key algorithm" "$work/out" || fail "an ECC key didn't say why"

# The committed key itself.
"$check" public docs/signing-key.asc >"$work/out" 2>&1 ||
  { cat "$work/out" >&2; fail "docs/signing-key.asc doesn't pass"; }

echo "signing-key-check-test: ok"
