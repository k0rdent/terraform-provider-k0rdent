#!/usr/bin/env bash
#
# Checks the release signing key, so a rotation that's due, a key about to
# expire, or a secret that doesn't match the published key turns up before a
# release needs it. A release finds out the same things, but only once its tag
# exists, and a tag can't be deleted.
#
#   check-signing-key.sh public KEYFILE
#     Reads the committed public key only, so it needs no secret. The nightly
#     runs it. Warns once the current signing subkey is past its rotation date,
#     or the parent past its own. Fails when either has under 90 days left.
#
#   check-signing-key.sh secret KEYFILE
#     What release-signing-check.yml runs, in the release-signing environment.
#     Imports GPG_PRIVATE_KEY into GNUPGHOME and fails unless:
#     - the parent is GPG_KEY_FINGERPRINT and KEYFILE's key
#     - the secret holds no usable parent. GitHub gets the signing subkey only
#     - the registry accepts the key's algorithm
#     - a test file signs with GPG_PASSPHRASE_FILE and verifies against
#       KEYFILE alone, bound to the fingerprint the way
#       validate-release-artifacts.sh binds it
#     - the subkey that signed is KEYFILE's current signing subkey, so a
#       rotation reached both GitHub and the committed key
#     Then it runs the public checks.
#
# The current signing subkey is the newest one that can sign and isn't revoked
# or expired. It's the one gpg signs with.
#
# Rotation is at 1 year for a subkey and 5 years for the parent, well before
# their 2 and 10 year expiries, so there's room for things to go wrong.
# SIGNING_KEY_NOW, in seconds since the epoch, stands in for the clock in tests.
# With SIGNING_KEY_DUE_FILE set, each key that's due to rotate is appended to
# that file as one tab-separated line: which key, its fingerprint, its
# rotation date and its expiry date. scripts/signing-key-reminder.sh reads it.
set -euo pipefail

day=86400
subkey_rotate_days=365
parent_rotate_days=1826
min_days_left=90
now="${SIGNING_KEY_NOW:-$(date +%s)}"

usage() { echo "usage: check-signing-key.sh public|secret KEYFILE" >&2; exit 2; }
mode="${1:-}"
keyfile="${2:-}"
case "$mode" in public | secret) ;; *) usage ;; esac
[ -n "$keyfile" ] || usage

problems=0
error() { echo "::error title=$1::$2" >&2; problems=$((problems + 1)); }
warning() { echo "::warning title=$1::$2"; }
# GNU date takes -d @SECONDS, BSD date takes -r SECONDS.
on() { date -u -d "@$1" +%F 2>/dev/null || date -u -r "$1" +%F; }

[ -f "$keyfile" ] || { error "No signing key file" "$keyfile doesn't exist."; exit 1; }

# One line per key in KEYFILE: kind fingerprint algorithm created expires
# validity capabilities. --show-keys reads the file without importing it.
# The listing is taken whole before awk reads it, so a gpg failure stops here.
listing="$(gpg --batch --with-colons --fixed-list-mode --show-keys "$keyfile" 2>/dev/null)" ||
  { error "Unreadable signing key" "gpg couldn't read $keyfile."; exit 1; }
keys="$(awk -F: '
  $1 == "pub" || $1 == "sub" {
    kind = $1; algo = $4; created = $6; expires = ($7 == "" ? "never" : $7)
    validity = ($2 == "" ? "-" : $2); caps = ($12 == "" ? "-" : $12); want = 1; next
  }
  $1 == "fpr" && want { print kind, $10, algo, created, expires, validity, caps; want = 0 }
' <<<"$listing")"

parents="$(awk '$1 == "pub"' <<<"$keys")"
if [ "$(awk 'END { print NR }' <<<"$parents")" -ne 1 ] || [ -z "$parents" ]; then
  error "Not one signing key" "$keyfile has to hold exactly one key."
  exit 1
fi
read -r _ parent_fpr parent_algo parent_created parent_expires _ _ <<<"$parents"

# The newest subkey that can sign and is neither revoked nor expired.
current="$(awk -v now="$now" '
  $1 == "sub" && $7 ~ /s/ && $6 !~ /[re]/ && ($5 == "never" || $5 > now) && $4 > best { best = $4; line = $0 }
  END { print line }' <<<"$keys")"

accepted_algorithm() {
  # registry.terraform.io accepts RSA (1, 2, 3) and DSA (17), and rejects
  # ECC, which gpg offers by default. Algorithm ids are from gpg's DETAILS.
  case "$1" in 1 | 2 | 3 | 17) return 0 ;; *) return 1 ;; esac
}

if [ "$mode" = secret ]; then
  for name in GNUPGHOME GPG_PRIVATE_KEY GPG_KEY_FINGERPRINT GPG_PASSPHRASE_FILE; do
    if [ -z "${!name:-}" ]; then
      error "Signing secret missing" "$name is not set."
    fi
  done
  [ "$problems" -eq 0 ] || exit 1
  [ -f "$GPG_PASSPHRASE_FILE" ] || { error "Signing secret missing" "GPG_PASSPHRASE_FILE names no file."; exit 1; }
  # A fingerprint pasted with spaces or in lowercase still signs, so it's
  # normalized before it's compared.
  fpr="$(printf '%s' "$GPG_KEY_FINGERPRINT" | tr -d '[:space:]' | tr '[:lower:]' '[:upper:]')"
  if [ "$fpr" != "$parent_fpr" ]; then
    error "Fingerprint doesn't match" "TERRAFORM_PROVIDER_GPG_KEY_FINGERPRINT is $fpr, and $keyfile holds $parent_fpr."
    exit 1
  fi

  printf '%s' "$GPG_PRIVATE_KEY" | gpg --batch --import 2>/dev/null ||
    { error "Signing key didn't import" "gpg couldn't import TERRAFORM_PROVIDER_GPG_PRIVATE_KEY."; exit 1; }
  secret="$(gpg --batch --with-colons --fixed-list-mode --list-secret-keys "$fpr" 2>/dev/null)" ||
    { error "Signing key not in the secret" "TERRAFORM_PROVIDER_GPG_PRIVATE_KEY holds no key $fpr."; exit 1; }

  # Field 15 of a sec record is # when only a stub of the parent is there.
  # A full parent in GitHub could make new subkeys, so anyone who reads the
  # secret could keep signing after a rotation.
  if ! awk -F: '$1 == "sec" && $15 != "#" { full = 1 } END { exit full }' <<<"$secret"; then
    error "Parent key in GitHub" "TERRAFORM_PROVIDER_GPG_PRIVATE_KEY holds the parent's secret part. Put the signing subkey only in GitHub: gpg --export-secret-subkeys."
  fi
  accepted_algorithm "$(awk -F: '$1 == "sec" { print $4; exit }' <<<"$secret")" ||
    error "Unsupported signing key algorithm" "The registry accepts RSA (1, 2, 3) and DSA (17)."

  work="$(mktemp -d)"
  verifier="$work/public"
  cleanup() {
    gpgconf --homedir "$verifier" --kill all >/dev/null 2>&1 || true
    rm -rf "$work"
  }
  trap cleanup EXIT
  install -d -m 700 "$verifier"
  printf 'release signing check %s\n' "$now" >"$work/probe"
  if ! gpg --batch --yes --pinentry-mode loopback --passphrase-file "$GPG_PASSPHRASE_FILE" \
    --local-user "$fpr" --output "$work/probe.sig" --detach-sign "$work/probe" 2>"$work/sign.log"; then
    error "Signing failed" "gpg couldn't sign with $fpr: $(tail -n 1 "$work/sign.log")"
    exit 1
  fi

  # Verified with the committed key alone, as someone installing a release
  # would. VALIDSIG's first field is the key that signed, and its last the
  # primary key.
  gpg --batch --homedir "$verifier" --import "$keyfile" 2>/dev/null
  status="$(gpg --batch --homedir "$verifier" --status-fd=1 --verify "$work/probe.sig" "$work/probe" 2>/dev/null || true)"
  validsig="$(awk -v fpr="$fpr" '$1 == "[GNUPG:]" && $2 == "VALIDSIG" && ($3 == fpr || $NF == fpr)' <<<"$status")"
  if [ -z "$validsig" ]; then
    # Against the secret's own keyring it names the subkey that signed, which
    # says whether the committed key is just missing a new subkey.
    own="$(gpg --batch --status-fd=1 --verify "$work/probe.sig" "$work/probe" 2>/dev/null || true)"
    signer="$(awk -v fpr="$fpr" '$1 == "[GNUPG:]" && $2 == "VALIDSIG" && $NF == fpr { print $3 }' <<<"$own")"
    if [ -n "$signer" ] && ! awk -v s="$signer" '$2 == s { found = 1 } END { exit !found }' <<<"$keys"; then
      error "Subkey not committed" "GitHub signs with subkey $signer, which $keyfile doesn't hold. Commit the public key from the vault."
    else
      error "Signature doesn't verify" "A signature made with the secret doesn't verify against $keyfile as a signature from $fpr."
    fi
  else
    read -r _ _ signer _ _ _ _ _ signer_algo _ <<<"$validsig"
    accepted_algorithm "$signer_algo" ||
      error "Unsupported signing key algorithm" "Subkey $signer is algorithm $signer_algo. The registry accepts RSA (1, 2, 3) and DSA (17)."
    if [ -z "$current" ]; then
      : # Reported below.
    elif [ "$signer" != "$(awk '{ print $2 }' <<<"$current")" ]; then
      if awk -v s="$signer" '$1 == "sub" && $2 == s { found = 1 } END { exit !found }' <<<"$keys"; then
        error "GitHub has an old subkey" "GitHub signs with subkey $signer, and $keyfile's current signing subkey is $(awk '{ print $2 }' <<<"$current"). Put the current subkey in TERRAFORM_PROVIDER_GPG_PRIVATE_KEY."
      else
        error "Subkey not committed" "GitHub signs with subkey $signer, which $keyfile doesn't hold. Commit the public key from the vault."
      fi
    fi
  fi
fi

# The public checks, in both modes.
accepted_algorithm "$parent_algo" ||
  error "Unsupported signing key algorithm" "$parent_fpr is algorithm $parent_algo. The registry accepts RSA (1, 2, 3) and DSA (17)."

# check_dates WHAT FPR CREATED EXPIRES ROTATE_DAYS
check_dates() {
  local what="$1" fpr="$2" created="$3" expires="$4" rotate_days="$5"
  local rotate_at=$((created + rotate_days * day))
  if [ "$expires" = never ]; then
    warning "No expiry" "The $what $fpr never expires. A lost key would stay trusted for good."
  else
    local left=$(((expires - now) / day))
    if [ "$left" -lt "$min_days_left" ]; then
      error "Signing key near expiry" "The $what $fpr expires on $(on "$expires"), in $left days. Rotate it now: see docs/release-signing.md."
    fi
  fi
  if [ "$now" -ge "$rotate_at" ]; then
    warning "Signing key due to rotate" "The $what $fpr was due to rotate on $(on "$rotate_at"). See docs/release-signing.md."
    if [ -n "${SIGNING_KEY_DUE_FILE:-}" ]; then
      local expiry=never
      [ "$expires" = never ] || expiry="$(on "$expires")"
      printf '%s\t%s\t%s\t%s\n' "$what" "$fpr" "$(on "$rotate_at")" "$expiry" >>"$SIGNING_KEY_DUE_FILE"
    fi
  fi
}

check_dates parent "$parent_fpr" "$parent_created" "$parent_expires" "$parent_rotate_days"
if [ -z "$current" ]; then
  error "No signing subkey" "$keyfile has no subkey that can still sign. Releases can't be signed until one is added."
else
  read -r _ sub_fpr _ sub_created sub_expires _ _ <<<"$current"
  check_dates "signing subkey" "$sub_fpr" "$sub_created" "$sub_expires" "$subkey_rotate_days"
fi

if [ "$problems" -gt 0 ]; then
  echo "signing key: $problems problem(s)" >&2
  exit 1
fi
summary="signing key: parent $parent_fpr"
if [ "$parent_expires" != never ]; then summary+=" expires $(on "$parent_expires")"; fi
summary+=", signing subkey $sub_fpr"
if [ "$sub_expires" != never ]; then summary+=" expires $(on "$sub_expires")"; fi
summary+=", rotate by $(on $((sub_created + subkey_rotate_days * day)))"
if [ "$mode" = secret ]; then summary+=". The secret signs, holds no parent, and matches $keyfile"; fi
echo "$summary"
