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
passphrase=throwaway-passphrase
printf '%s' "$passphrase" >"$work/passphrase"

# make_key NAME [PARENT_ALGO] [PARENT_EXPIRY] [SUBKEY_EXPIRY]: a parent that
# only certifies, 10 years, and a signing subkey, 2 years, the shape the
# release key has. Prints nothing; leaves the keyring in $work/NAME.
make_key() {
  local home="$work/$1" algo="${2:-rsa2048}"
  install -d -m 700 "$home"
  gpg --homedir "$home" --batch --pinentry-mode loopback --passphrase "$passphrase" \
    --quick-generate-key "throwaway $1 <$1@example.invalid>" "$algo" cert "${3:-10y}" 2>/dev/null
  add_subkey "$1" rsa2048 sign "${4:-2y}"
}

# add_subkey NAME ALGO USAGE EXPIRY
add_subkey() {
  gpg --homedir "$work/$1" --batch --pinentry-mode loopback --passphrase "$passphrase" \
    --quick-add-key "$(fpr_of "$1")" "$2" "$3" "$4" 2>/dev/null
}

# subkey_fprs NAME: every subkey's fingerprint, oldest first.
subkey_fprs() { gpg --homedir "$work/$1" --with-colons --list-keys 2>/dev/null | awk -F: '$1 == "sub" { s = 1; next } s && $1 == "fpr" { print $10; s = 0 }'; }
fpr_of() { gpg --homedir "$work/$1" --with-colons --list-keys 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }'; }
public_of() { gpg --homedir "$work/$1" --armor --export "$(fpr_of "$1")" 2>/dev/null; }
subkeys_of() { gpg --homedir "$work/$1" --batch --pinentry-mode loopback --passphrase "$passphrase" --armor --export-secret-subkeys "$(fpr_of "$1")"; }
full_of() { gpg --homedir "$work/$1" --batch --pinentry-mode loopback --passphrase "$passphrase" --armor --export-secret-keys "$(fpr_of "$1")"; }
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

# A trailing newline, which the release's gpg --local-user doesn't accept.
expect_fail "holds a newline" \
  run_secret "$work/out" "$work/good.asc" "$(subkeys_of good)" "$good_fpr"$'\n'

# A passphrase pasted into the fingerprint secret. Nothing like it reaches the
# log: not as set, not uppercased, not with its spaces dropped.
leak="my secret pass phrase"
expect_fail "isn't a fingerprint" \
  run_secret "$work/out" "$work/good.asc" "$(subkeys_of good)" "$leak"
for form in "$leak" "MY SECRET PASS PHRASE" mysecretpassphrase MYSECRETPASSPHRASE; do
  grep -Fiq "$form" "$work/out" && { cat "$work/out" >&2; fail "the check printed the fingerprint secret as $form"; }
done

# A hex value of the wrong key isn't printed either.
expect_fail "doesn't name" \
  run_secret "$work/out" "$work/good.asc" "$(subkeys_of good)" "0123456789ABCDEF0123456789ABCDEF01234567"
grep -Fq "0123456789ABCDEF" "$work/out" && { cat "$work/out" >&2; fail "the check printed a mismatched fingerprint"; }

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
expect_fail "doesn't name" \
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
gpg --homedir "$work/good" --batch --pinentry-mode loopback --passphrase "$passphrase" \
  --quick-add-key "$good_fpr" rsa2048 sign 2y 2>/dev/null
public_of good >"$work/rotated.asc"
expect_fail "GitHub has an old subkey" \
  run_secret "$work/out" "$work/rotated.asc" "$old_secret" "$good_fpr"
expect_fail "Subkey not committed" \
  run_secret "$work/out" "$old_public" "$(subkeys_of good)" "$good_fpr"
run_secret "$work/out" "$work/rotated.asc" "$(subkeys_of good)" "$good_fpr" ||
  { cat "$work/out" >&2; fail "a rotation that reached both sides was refused"; }

# The current signing subkey is the newest one that signs and isn't revoked.
# A newer subkey made for encryption, or a newer signing subkey that was
# revoked, doesn't count. "other" is done with above, so it's reused.
other_sign="$(subkey_fprs other | head -n 1)"
sleep 1
add_subkey other rsa2048 encr 2y
public_of other >"$work/other-encr.asc"
"$check" public "$work/other-encr.asc" >"$work/out" 2>&1 || { cat "$work/out" >&2; fail "a key with an encryption subkey was refused"; }
grep -Fq "signing subkey $other_sign" "$work/out" || { cat "$work/out" >&2; fail "a newer encryption subkey was taken for the signing subkey"; }

sleep 1
add_subkey other rsa2048 sign 2y
revoked="$(subkey_fprs other | tail -n 1)"
printf 'key 3\nrevkey\ny\n0\n\ny\nsave\n' |
  gpg --homedir "$work/other" --batch --pinentry-mode loopback --passphrase "$passphrase" \
    --command-fd 0 --status-fd 2 --edit-key "$(fpr_of other)" 2>/dev/null
public_of other >"$work/other-revoked.asc"
gpg --batch --with-colons --show-keys "$work/other-revoked.asc" 2>/dev/null | grep -q '^sub:r:' ||
  fail "the test couldn't revoke a subkey"
"$check" public "$work/other-revoked.asc" >"$work/out" 2>&1 || { cat "$work/out" >&2; fail "a key with a revoked subkey was refused"; }
grep -Fq "signing subkey $other_sign" "$work/out" || { cat "$work/out" >&2; fail "a revoked subkey ($revoked) was taken for the signing subkey"; }

# An ECC signing subkey under an RSA parent. The registry verifies with the
# subkey that signed, so it's refused both ways, and the secret check says
# GitHub signs with it.
sleep 1
add_subkey good ed25519 sign 2y
public_of good >"$work/ecc-sub.asc"
"$check" public "$work/ecc-sub.asc" >"$work/out" 2>&1 && { cat "$work/out" >&2; fail "an ECC signing subkey passed"; }
grep -Fq "The signing subkey" "$work/out" || { cat "$work/out" >&2; fail "an ECC signing subkey didn't say why"; }
expect_fail "GitHub signs with subkey" \
  run_secret "$work/out" "$work/ecc-sub.asc" "$(subkeys_of good)" "$good_fpr"

# A missing secret.
expect_fail "GPG_PRIVATE_KEY is not set" \
  run_secret "$work/out" "$work/good.asc" "" "$good_fpr"

# An ECC parent, which gpg offers by default and the registry rejects.
make_key ecc ed25519
public_of ecc >"$work/ecc.asc"
"$check" public "$work/ecc.asc" >"$work/out" 2>&1 && { cat "$work/out" >&2; fail "an ECC key passed"; }
grep -Fq "Unsupported signing key algorithm" "$work/out" || fail "an ECC key didn't say why"

# The parent's own dates, with a subkey that outlives the clock below. Past 5
# years it's due to rotate, a warning that reaches the due file. Under 90
# days from its 10 years it fails.
make_key long rsa2048 10y 11y
public_of long >"$work/long.asc"
long_created="$(gpg --batch --with-colons --show-keys "$work/long.asc" 2>/dev/null | awk -F: '$1 == "pub" { print $6 }')"
long_fpr="$(fpr_of long)"
: >"$work/due"
SIGNING_KEY_DUE_FILE="$work/due" SIGNING_KEY_NOW=$((long_created + 1830 * day)) "$check" public "$work/long.asc" >"$work/out" 2>&1 ||
  { cat "$work/out" >&2; fail "a parent past its rotation date failed instead of warning"; }
grep -Fq "The parent $long_fpr was due to rotate" "$work/out" || { cat "$work/out" >&2; fail "a parent past 5 years didn't warn"; }
grep -q "^parent	$long_fpr	" "$work/due" || { cat "$work/due" >&2; fail "a rotation-due parent wasn't written to the due file"; }

SIGNING_KEY_NOW=$((long_created + (3650 - 30) * day)) "$check" public "$work/long.asc" >"$work/out" 2>&1 &&
  { cat "$work/out" >&2; fail "a parent 30 days from expiry passed"; }
grep -Fq "The parent $long_fpr expires" "$work/out" || { cat "$work/out" >&2; fail "a parent near expiry didn't say so"; }

# A key that never expires: allowed, since the registry takes it, but warned,
# since a lost key would stay trusted for good.
make_key forever rsa2048 never never
public_of forever >"$work/forever.asc"
"$check" public "$work/forever.asc" >"$work/out" 2>&1 || { cat "$work/out" >&2; fail "a key with no expiry failed"; }
grep -Fq "::warning title=No expiry::" "$work/out" || { cat "$work/out" >&2; fail "a key with no expiry didn't warn"; }

# What's due reaches the reminder: one line per key past its rotation date,
# none for a fresh key, and the near-expiry failure still writes its line.
: >"$work/due"
SIGNING_KEY_DUE_FILE="$work/due" "$check" public "$work/good.asc" >"$work/out" 2>&1 || fail "a fresh key failed"
[ ! -s "$work/due" ] || { cat "$work/due" >&2; fail "a fresh key was reported due"; }

SIGNING_KEY_DUE_FILE="$work/due" SIGNING_KEY_NOW=$((created + 400 * day)) "$check" public "$work/good.asc" >"$work/out" 2>&1 ||
  fail "a key past its rotation date failed"
grep -q "^signing subkey	$(awk -F: '$1=="fpr"{f=$10} END{print f}' <(gpg --batch --with-colons --show-keys "$work/good.asc" 2>/dev/null))	" "$work/due" ||
  { cat "$work/due" >&2; fail "a rotation-due subkey wasn't written to the due file"; }

: >"$work/due"
SIGNING_KEY_DUE_FILE="$work/due" SIGNING_KEY_NOW=$((created + 700 * day)) "$check" public "$work/good.asc" >"$work/out" 2>&1 &&
  fail "a subkey near expiry passed"
grep -q '^signing subkey	' "$work/due" || fail "a subkey near expiry wasn't written to the due file"

# The reminder, against a stub gh that keeps each issue's state in a
# directory: its author, whether it's open, its title and body, and how many
# pointer comments it has.
stub="$work/stub"
mkdir -p "$stub"
cat >"$stub/gh" <<'STUB'
#!/usr/bin/env bash
state="$GH_STATE"
echo "$*" >>"$state/calls"
flag() { local want="$1"; shift; while [ $# -gt 0 ]; do [ "$1" = "$want" ] && { printf '%s' "$2"; return; }; shift; done; }
case "$1" in
  label) exit 0 ;;
  api)
    case "$2" in
      */comments*) n="${2#*/issues/}"; n="${n%%/*}"; cat "$state/issues/$n/pointers" 2>/dev/null || echo 0 ;;
      *) for d in $(ls "$state/issues" 2>/dev/null | sort -n); do
           [ -f "$state/issues/$d/open" ] && printf '%s\t%s\n' "$d" "$(cat "$state/issues/$d/author")"
         done ;;
    esac
    exit 0 ;;
esac
case "$1 $2" in
  "issue create")
    n=$(( $(ls "$state/issues" 2>/dev/null | sort -n | tail -n 1 || true) + 0 ))
    n=$(( n < 7 ? 7 : n + 1 ))
    mkdir -p "$state/issues/$n"
    echo "github-actions[bot]" >"$state/issues/$n/author"
    : >"$state/issues/$n/open"
    flag --title "$@" >"$state/issues/$n/title"
    flag --body "$@" >"$state/issues/$n/body"
    echo "https://github.com/k0rdent/terraform-provider-k0rdent/issues/$n" ;;
  "issue view") cat "$state/issues/$3/title" ;;
  "issue edit") flag --title "$@" >"$state/issues/$3/title"; flag --body "$@" >"$state/issues/$3/body" ;;
  "issue comment")
    case "$(flag --body "$@")" in
      *"signing-key-rotation pointer"*) echo $(( $(cat "$state/issues/$3/pointers" 2>/dev/null || echo 0) + 1 )) >"$state/issues/$3/pointers" ;;
    esac ;;
  "issue close") rm -f "$state/issues/$3/open" ;;
esac
STUB
chmod +x "$stub/gh"

# remind RESULT DUE_FILE runs the reminder against the stub gh.
remind() {
  GH_STATE="$work/gh" GH_REPO=k0rdent/terraform-provider-k0rdent PATH="$stub:$PATH" RESULT="$1" RUN_URL=https://example.invalid/run \
    "$PWD/scripts/signing-key-reminder.sh" "$2" >"$work/out" 2>&1 || { cat "$work/out" >&2; fail "the reminder failed"; }
}

calls() { grep -c "^issue $1" "$work/gh/calls" || true; }
# add_issue NUMBER AUTHOR: an open issue carrying the label.
add_issue() { mkdir -p "$work/gh/issues/$1" && echo "$2" >"$work/gh/issues/$1/author" && : >"$work/gh/issues/$1/open" && echo "theirs" >"$work/gh/issues/$1/title"; }
is_open() { [ -f "$work/gh/issues/$1/open" ]; }
pointers() { cat "$work/gh/issues/$1/pointers" 2>/dev/null || echo 0; }
fresh() { rm -rf "$work/gh" && mkdir -p "$work/gh/issues" && : >"$work/gh/calls"; }

fresh
printf 'signing subkey\tABCD\t2027-10-23\t2028-10-22\n' >"$work/due1"

# Due, nothing open: one issue, titled with the date, saying where the how-to is.
remind failure "$work/due1"
[ "$(calls create)" = 1 ] || fail "a due key didn't open an issue"
grep -Fxq "Rotate the release signing subkey (due 2027-10-23)" "$work/gh/issues/7/title" || { cat "$work/gh/issues/7/title" >&2; fail "wrong issue title"; }
for needle in ABCD "rotate by 2027-10-23" "expires 2028-10-22" "the maintainers' runbook, kept with the key"; do
  grep -Fq "$needle" "$work/gh/issues/7/body" || { cat "$work/gh/issues/7/body" >&2; fail "the issue body doesn't say $needle"; }
done
grep -Eq '@[A-Za-z0-9]' "$work/gh/issues/7/body" && fail "the issue body mentions someone"

# Due again the next night: the same issue, edited, with no comment and no
# second issue.
remind success "$work/due1"
[ "$(calls create)" = 1 ] || fail "a second night opened a duplicate issue"
[ "$(calls edit)" = 1 ] || fail "a second night didn't update the open issue"
[ "$(calls comment)" = 0 ] || fail "an unchanged reminder commented"

# The parent comes due too: the title changes, and that comments once.
printf 'parent\tEF01\t2031-10-23\t2036-10-20\nsigning subkey\tABCD\t2027-10-23\t2028-10-22\n' >"$work/due2"
remind success "$work/due2"
grep -Fxq "Rotate the release signing parent and subkey (due 2027-10-23)" "$work/gh/issues/7/title" || { cat "$work/gh/issues/7/title" >&2; fail "wrong title for two keys"; }
[ "$(calls comment)" = 1 ] || fail "a changed reminder didn't comment"
[ "$(calls create)" = 1 ] || fail "a changed reminder opened a duplicate"

# A check that broke without naming anything due leaves the issue alone.
: >"$work/none"
remind failure "$work/none"
[ "$(calls close)" = 0 ] || fail "a failed check closed the reminder"

# Rotated: a clean check closes it.
remind success "$work/none"
[ "$(calls close)" = 1 ] || fail "a clean check didn't close the reminder"
is_open 7 && fail "the reminder is still open"

# Clean with nothing open: nothing happens.
remind success "$work/none"
[ "$(calls close)" = 1 ] || fail "a clean check with nothing open closed something"

# Someone else put the label on an issue. It's never adopted, edited or
# closed. The workflow opens its own and points there once.
fresh
add_issue 3 someone
remind failure "$work/due1"
[ "$(calls create)" = 1 ] || fail "a labeled issue someone else opened stopped the reminder opening its own"
grep -q '^issue edit 3 ' "$work/gh/calls" && fail "the reminder edited someone else's issue"
[ "$(pointers 3)" = 1 ] || fail "someone else's labeled issue wasn't pointed to the reminder"
remind success "$work/due1"
[ "$(pointers 3)" = 1 ] || fail "someone else's labeled issue was pointed to the reminder twice"
grep -q '^issue edit 3 ' "$work/gh/calls" && fail "the reminder edited someone else's issue"
remind success "$work/none"
is_open 3 || fail "the reminder closed someone else's issue"
is_open 7 && fail "the reminder's own issue didn't close"

# Two of the workflow's own: the oldest is managed, the other pointed to it
# once and otherwise left alone.
fresh
add_issue 10 "github-actions[bot]"
add_issue 11 "github-actions[bot]"
remind success "$work/due1"
[ "$(calls create)" = 0 ] || fail "two open reminders led to a third"
grep -q '^issue edit 10 ' "$work/gh/calls" || fail "the oldest reminder wasn't the one updated"
grep -q '^issue edit 11 ' "$work/gh/calls" && fail "the newer reminder was edited"
[ "$(pointers 11)" = 1 ] || fail "the newer reminder wasn't pointed to the oldest"
remind success "$work/due1"
[ "$(pointers 11)" = 1 ] || fail "the newer reminder was pointed twice"
remind success "$work/none"
is_open 11 || fail "the newer reminder was closed"
is_open 10 && fail "the managed reminder didn't close"

# The committed key itself.
"$check" public docs/signing-key.asc >"$work/out" 2>&1 ||
  { cat "$work/out" >&2; fail "docs/signing-key.asc doesn't pass"; }

echo "signing-key-check-test: ok"
