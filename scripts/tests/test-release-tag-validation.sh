#!/usr/bin/env bash
# Runs the release dispatch's tag validation, the script itself as the
# workflow holds it, against a throwaway repository and a stand-in for gh. A
# check that reads the workflow's text can say the release-branch checks are
# there. Only running them shows that each one refuses what it is for, and
# fails closed when GitHub doesn't answer.
set -euo pipefail
cd "$(dirname "$0")/../.."
repo="$PWD"
fail() { echo "release tag validation: $*" >&2; exit 1; }

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The step's script, cut from the workflow by its indentation.
awk '
  /^      - id: validate$/ { step = 1 }
  step && /^        run: \|$/ { body = 1; next }
  body && /^          / { sub(/^          /, ""); print; next }
  body && /^$/ { print ""; next }
  body { exit }
' .github/workflows/release-dispatch.yml > "$work/validate.sh"
grep -q 'commits/$commit/pulls' "$work/validate.sh" || fail "could not cut the validation script out of release-dispatch.yml"

# Nothing in the background: the long history below would start gc or
# maintenance after a fetch, and it would rewrite a clone while the next case
# removes and remakes it.
export GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=gc.auto GIT_CONFIG_VALUE_0=0 \
  GIT_CONFIG_KEY_1=maintenance.auto GIT_CONFIG_VALUE_1=false
g() { git -c user.name=t -c user.email=t@example.invalid -c commit.gpgsign=false -c tag.gpgsign=false "$@"; }

# origin: main with a merged feature branch, and release-v0.1.x cut from main.
git init -q -b main "$work/origin"
cd "$work/origin"
g commit -q --allow-empty -m first
# A long history, so rev-list prints more than a pipe holds. A check that
# stops reading at the first match then kills rev-list with SIGPIPE, and
# under pipefail that reads as a failure. A short history would hide it.
tree="$(git rev-parse 'HEAD^{tree}')"
head="$(git rev-parse HEAD)"
for i in $(seq 1 2000); do head="$(g commit-tree "$tree" -p "$head" -m "history $i")"; done
git update-ref refs/heads/main "$head"
git reset -q --hard
g checkout -q -b feature; g commit -q --allow-empty -m feature; feature="$(git rev-parse HEAD)"
g checkout -q main; g merge -q --no-ff -m "merge feature" feature
g tag v0.1.0
g checkout -q -b release-v0.1.x
g checkout -q -b backport; g commit -q --allow-empty -m fix
g checkout -q release-v0.1.x; g merge -q --no-ff -m "merge backport" backport
merged="$(git rev-parse HEAD)"
g tag v0.1.1
g commit -q --allow-empty -m "pushed straight to the branch"
g tag v0.1.2
g tag v0.2.0 "$merged"
# A release branch cut from inside a merged feature branch, not from main.
g checkout -q -b release-v0.3.x "$feature"
g checkout -q -b backport3; g commit -q --allow-empty -m fix3
g checkout -q release-v0.3.x; g merge -q --no-ff -m "merge backport3" backport3
off_main="$(git rev-parse HEAD)"
g tag v0.3.1
g checkout -q main
# The typo from RELEASE.md's major gate: v1.0.0-rc.1 pushed for v0.1.0-rc.1.
# Tags can't be deleted, so it stays on main forever.
g commit -q --allow-empty -m "mistyped candidate"
g tag v1.0.0-rc.1
g commit -q --allow-empty -m "the real v1"
g tag v1.0.0

mkdir -p "$work/bin" "$work/stub"
cat > "$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
# gh api [--paginate] PATH [--jq EXPR]: answers from $STUB, or fails like an API error.
set -euo pipefail
[ "$1" = api ] || exit 2
shift
path=""
jq_expr=.
while [ $# -gt 0 ]; do
  case "$1" in
    --jq) jq_expr="$2"; shift 2 ;;
    -*) shift ;;
    *) path="$1"; shift ;;
  esac
done
[ -e "$STUB/fail" ] && { echo "gh: HTTP 502" >&2; exit 1; }
case "$path" in
  */rules/branches/*) file="$STUB/rules.json" ;;
  */releases\?*) file="$STUB/releases.json"; [ -e "$file" ] || echo '[]' > "$file" ;;
  */commits/*/pulls) sha="${path%/pulls}"; sha="${sha##*/}"; file="$STUB/pulls-$sha.json"; [ -e "$file" ] || echo '[]' > "$file" ;;
  */pulls/*/reviews) n="${path%/reviews}"; n="${n##*/}"; file="$STUB/reviews-$n.json"; [ -e "$file" ] || echo '[]' > "$file" ;;
  *) exit 2 ;;
esac
jq -r "$jq_expr" "$file"
EOF
chmod +x "$work/bin/gh"

full_rules='[{"type":"deletion"},{"type":"non_fast_forward"},{"type":"required_signatures"},
  {"type":"pull_request","parameters":{"required_approving_review_count":1}},
  {"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"CI"}]}}]'
# pr_merge BRANCH SHA [APPROVER]: a pull request by "author", number 7,
# merged into BRANCH as SHA, approved by APPROVER (a reviewer by default).
pr_merge() {
  printf '[{"number":7,"user":{"login":"author"},"merged_at":"2026-01-01T00:00:00Z","base":{"ref":"%s"},"merge_commit_sha":"%s"}]' "$1" "$2" > "$work/stub/pulls-$2.json"
  printf '[{"state":"COMMENTED","user":{"login":"someone"}},{"state":"APPROVED","user":{"login":"%s"}}]' "${3:-reviewer}" > "$work/stub/reviews-7.json"
}

# validate TAG: clone origin fresh, run the step's script, report its status.
validate() {
  rm -rf "$work/clone"
  git clone -q "$work/origin" "$work/clone"
  mkdir -p "$work/clone/scripts"
  cp "$repo/scripts/check-major-release.sh" "$work/clone/scripts/"
  (cd "$work/clone" && PATH="$work/bin:$PATH" STUB="$work/stub" GITHUB_REF=refs/heads/main \
    GITHUB_REPOSITORY=k0rdent/test GITHUB_OUTPUT="$work/out" RELEASE_TAG="$1" ALLOW_MAJOR=0 \
    bash "$work/validate.sh") > "$work/log" 2>&1
}
accepted() { validate "$1" || { cat "$work/log" >&2; fail "$1 was refused ($2)"; }; }
refused() {
  if validate "$1"; then fail "$1 was accepted ($2)"; fi
  grep -Fq "$3" "$work/log" || { cat "$work/log" >&2; fail "$1 was refused, but not for $2"; }
}
reset() { rm -f "$work/stub/"*; echo "$full_rules" > "$work/stub/rules.json"; pr_merge release-v0.1.x "$merged"; pr_merge release-v0.3.x "$off_main"; }

reset
accepted v0.1.0 "a tag on main"
accepted v0.1.1 "a pull request's merge on a protected release branch"
refused v0.1.2 "a commit pushed straight to the release branch" "is not the merge of a pull request"
refused v0.2.0 "a v0.2 tag on release-v0.1.x" "neither origin/main nor origin/release-v0.2.x"
refused v0.3.1 "a release branch that doesn't start from main's own history" "doesn't start from main"

for rule in pull_request required_signatures required_status_checks non_fast_forward deletion; do
  reset
  jq --arg r "$rule" 'map(select(.type != $r))' <<< "$full_rules" > "$work/stub/rules.json"
  refused v0.1.1 "a release branch without $rule" "has no $rule rule"
done

reset
jq 'map(if .type == "pull_request" then .parameters.required_approving_review_count = 0 else . end)' <<< "$full_rules" > "$work/stub/rules.json"
refused v0.1.1 "a pull request rule with no approval" "without an approval"

reset
jq 'map(if .type == "required_status_checks" then .parameters.required_status_checks = [{"context":"lint"}] else . end)' <<< "$full_rules" > "$work/stub/rules.json"
refused v0.1.1 "required checks without CI" "doesn't require the CI check"

reset
pr_merge main "$merged"
refused v0.1.1 "a merge whose pull request went to another branch" "is not the merge of a pull request"

reset
echo '[{"state":"COMMENTED","user":{"login":"reviewer"}}]' > "$work/stub/reviews-7.json"
refused v0.1.1 "a pull request nobody approved" "nobody but its author approved"

reset
pr_merge release-v0.1.x "$merged" author
refused v0.1.1 "a pull request only its author approved" "nobody but its author approved"

reset
touch "$work/stub/fail"
if validate v0.1.1; then fail "v0.1.1 was accepted when GitHub's API failed"; fi

reset
echo '[]' > "$work/stub/rules.json"
refused v0.1.1 "an empty rules answer" "has no pull_request rule"

# The major gate counts only releases this workflow published. The mistyped
# v1.0.0-rc.1 is a tag on main and nothing more, so v1.0.0 still needs the
# decision.
reset
refused v1.0.0 "a v1 tag that was never released" "raises the major version from 0 to 1"
# release BOT_OR_USER DRAFT TAG: one release in the releases answer.
release() { printf '[{"tag_name":"%s","draft":%s,"author":{"login":"%s"}}]' "$3" "$2" "$1" > "$work/stub/releases.json"; }
reset; release "github-actions[bot]" true v1.0.0-rc.1
refused v1.0.0 "a v1 that only got as far as a draft" "raises the major version from 0 to 1"
reset; release someone false v1.0.0-rc.1
refused v1.0.0 "a v1 release made by hand" "raises the major version from 0 to 1"
reset; release "github-actions[bot]" false v1.0.0-rc.1
accepted v1.0.0 "v1.0.0-rc.1 published by the workflow"

reset
touch "$work/stub/fail"
if validate v0.1.0; then fail "v0.1.0 was accepted when GitHub's releases API failed"; fi

echo "release tag validation: PASS"
