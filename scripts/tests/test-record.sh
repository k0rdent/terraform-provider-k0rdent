#!/usr/bin/env bash
#
# Tests scripts/record.sh with a stand-in terraform and tofu on a PATH made
# for the test: the transcript, one step directory per wrapped command with
# its streams kept apart, TF_LOG routed into the step, the fake's log sliced
# per command, crash.log moved, secrets redacted, the index, and the exit
# code handed back. Nothing here needs a network, a credential or a
# toolchain.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
record="$here/scripts/record.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "record test: FAIL - $1" >&2; exit 1; }
pass() { echo "record test: ok - $1"; }

# The stand-in: prints a line to each stream, writes TF_LOG_PATH when set with
# the credential in it as TRACE would, appends to the fake's log, and exits as
# the subcommand says. `apply` fails and leaves a crash.log, as Terraform
# would on a panic.
mkdir -p "$work/bin" "$work/project/sub"
cat > "$work/bin/terraform" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  output) echo '{"ok": true}' ;;
  version) echo "Terraform v9.9.9-stub"; echo "on linux_amd64" ;;
  *) echo "stub $(basename "$0") $*" ;;
esac
echo "stub warning on stderr for $*" >&2
if [ -n "${TF_LOG_PATH:-}" ]; then
  printf '[%s] log for %s with secret %s\n' "${TF_LOG:-unset}" "$*" "${K0RDENT_CLIENT_SECRET:-none}" >> "$TF_LOG_PATH"
else
  echo "no TF_LOG_PATH" >&2
fi
[ -z "${STUB_FAKE_LOG:-}" ] || echo "request for $*" >> "$STUB_FAKE_LOG"
case "${1:-}" in
  apply) echo "Error: stub apply failed" >&2; echo "panic: stub" > crash.log; exit 1 ;;
esac
EOF
chmod +x "$work/bin/terraform"
cp "$work/bin/terraform" "$work/bin/tofu"
chmod +x "$work/bin/tofu"

# The driver: what a walkthrough script does, with a pipe the shim must not
# pollute and an argument that needs quoting.
cat > "$work/project/drive.sh" <<'EOF'
#!/usr/bin/env bash
echo "driver start $1"
terraform plan
( cd sub && terraform apply -auto-approve ) || echo "apply exit $?"
terraform output -json > out.json
tofu plan
echo "driver end"
exit 3
EOF
chmod +x "$work/project/drive.sh"

export PATH="$work/bin:$PATH"
export K0RDENT_CLIENT_SECRET="s3cret-value-for-the-test"
export STUB_FAKE_LOG="$work/fake.log"
: > "$STUB_FAKE_LOG"
unset TF_LOG TF_LOG_PATH RECORD_OUT RECORD_TF_LOG RECORD_FAKE_LOG RECORD_WRAP RECORD_NO_PTY

# --- a whole run --------------------------------------------------------------

out="$work/run1"
status=0
( cd "$work/project" && "$record" --out "$out" --fake-log "$STUB_FAKE_LOG" -- ./drive.sh "a b'c" ) \
  > "$work/run1.stdout" 2> "$work/run1.stderr" </dev/null || status=$?
[ "$status" = 3 ] || { cat "$work/run1.stderr" >&2; fail "the driver exited 3, record.sh exited $status"; }
[ "$(cat "$out/exit-code")" = 3 ] || fail "exit-code holds $(cat "$out/exit-code"), want 3"
pass "the command's exit code comes back"

grep -q "driver start a b'c" "$out/terminal.txt" || fail "terminal.txt lacks the driver's first line with its quoted argument"
grep -q "driver end" "$out/terminal.txt" || fail "terminal.txt lacks the driver's last line"
grep -q "stub terraform plan" "$out/terminal.txt" || fail "terminal.txt lacks the wrapped command's stdout"
grep -q "stub warning on stderr for plan" "$out/terminal.txt" || fail "terminal.txt lacks the wrapped command's stderr"
grep -q "driver start" "$work/run1.stdout" || fail "the terminal no longer shows the command's output"
pass "the transcript has the terminal, and the terminal still shows everything"

[ -d "$out/steps/001-terraform-plan" ] || fail "no steps/001-terraform-plan: $(ls "$out/steps")"
[ -d "$out/steps/002-terraform-apply" ] || fail "no steps/002-terraform-apply: $(ls "$out/steps")"
[ -d "$out/steps/003-terraform-output" ] || fail "no steps/003-terraform-output: $(ls "$out/steps")"
[ -d "$out/steps/004-tofu-plan" ] || fail "no steps/004-tofu-plan: $(ls "$out/steps")"
pass "one step directory per wrapped command, numbered in order"

step="$out/steps/001-terraform-plan"
grep -qx "stub terraform plan" "$step/stdout.log" || fail "stdout.log lacks the command's stdout"
grep -q "stub warning" "$step/stdout.log" && fail "stdout.log holds stderr text"
grep -qx "stub warning on stderr for plan" "$step/stderr.log" || fail "stderr.log lacks the command's stderr"
grep -q "stub terraform" "$step/stderr.log" && fail "stderr.log holds stdout text"
grep -q "stub terraform plan" "$step/output.log" && grep -q "stub warning" "$step/output.log" \
  || fail "output.log lacks one of the streams"
grep -q "stub terraform plan" "$step/terminal.txt" || fail "the step's terminal.txt lacks its own output"
grep -q "driver start" "$step/terminal.txt" && fail "the step's terminal.txt reaches back before the command"
pass "stdout, stderr, both together, and the step's slice of the terminal"

[ "$(cat "$work/project/out.json")" = '{"ok": true}' ] \
  || fail "the shim changed what terraform output -json wrote to a pipe: $(cat "$work/project/out.json")"
pass "a wrapped command's stdout reaches the caller's pipe unchanged"

grep -q '^\[DEBUG\] log for plan' "$step/terraform.log" || fail "terraform.log lacks the TF_LOG output, or TF_LOG was not DEBUG"
grep -q '"tf_log": "DEBUG"' "$step/meta.json" || fail "meta.json does not record TF_LOG"
grep -q 'TF_LOG:    DEBUG, in terraform.log' "$step/command.txt" || fail "command.txt does not say where the log went"
grep -q 'no TF_LOG_PATH' "$step/stderr.log" && fail "TF_LOG_PATH was not set for the command"
pass "TF_LOG defaults to DEBUG and lands in the step's terraform.log"

if grep -rq "$K0RDENT_CLIENT_SECRET" "$out"; then
  grep -rl "$K0RDENT_CLIENT_SECRET" "$out" >&2
  fail "the secret is in the run directory"
fi
grep -q 'with secret \[redacted\]' "$step/terraform.log" || fail "terraform.log was not redacted"
grep -q '^K0RDENT_CLIENT_SECRET=\[redacted\]$' "$out/environment.txt" || fail "environment.txt shows the secret, or not the variable"
pass "the secret is redacted everywhere, and environment.txt names the variable"

apply="$out/steps/002-terraform-apply"
grep -q '"exit_code": 1' "$apply/meta.json" || fail "the apply's meta.json does not record exit 1"
grep -q 'exit code: 1' "$apply/command.txt" || fail "the apply's command.txt does not record exit 1"
[ "$(sed -n 's/^cwd: *//p' "$apply/command.txt")" = "$work/project/sub" ] || fail "the apply's cwd is wrong: $(grep cwd "$apply/command.txt")"
[ -f "$apply/crash.log" ] || fail "crash.log was not moved into the apply's step"
[ ! -e "$work/project/sub/crash.log" ] || fail "crash.log is still where terraform left it"
grep -q 'apply exit 1' "$out/terminal.txt" || fail "the driver did not see the apply's exit code"
pass "a failing command: its exit code, cwd and crash.log are in its step, and the caller saw the failure"

grep -qx 'request for plan' "$step/fake.log" || fail "the plan's fake.log lacks its request"
grep -q 'request for apply' "$step/fake.log" && fail "the plan's fake.log has the apply's request"
grep -qx 'request for apply -auto-approve' "$apply/fake.log" || fail "the apply's fake.log lacks its request"
grep -q 'request for plan' "$apply/fake.log" && fail "the apply's fake.log reaches back to the plan"
pass "the fake's log is sliced per command"

grep -q '^### 002-terraform-apply' "$out/index.md" || fail "index.md lacks the apply's section"
grep -q 'Error: stub apply failed' "$out/index.md" || fail "index.md lacks the apply's error line"
grep -q '4 wrapped commands, 1 with a non-zero exit code' "$out/index.md" || fail "index.md's count is wrong: $(grep wrapped "$out/index.md")"
[ "$(wc -l < "$out/steps.jsonl" | tr -d ' ')" = 4 ] || fail "steps.jsonl has $(wc -l < "$out/steps.jsonl") lines, want 4"
python3 -c 'import json, sys; [json.loads(l) for l in open(sys.argv[1])]' "$out/steps.jsonl" || fail "steps.jsonl is not JSON per line"
grep -q 'recorded 4 wrapped commands, 1 failed, exit code 3' "$work/run1.stderr" || fail "the summary line is wrong: $(grep record: "$work/run1.stderr")"
grep -q "record: $out" "$work/run1.stderr" || fail "the directory was not named at the end"
grep -q "^terraform:  Terraform v9.9.9-stub, $work/bin/terraform$" "$out/command.txt" || fail "command.txt lacks the terraform version: $(cat "$out/command.txt")"
pass "index.md, steps.jsonl, the summary and the versions"

[ ! -e "$out/.command" ] || fail "the argv file was left behind"
[ "$(stat -c %a "$out" 2>/dev/null || stat -f %Lp "$out")" = 700 ] || fail "the run directory is not 0700"
pass "the directory is private and tidy"

# --- the shell's own TF_LOG wins; off writes no log --------------------------

out="$work/run2"
( cd "$work/project" && TF_LOG=TRACE "$record" --out "$out" --fake-log "$STUB_FAKE_LOG" -- terraform plan ) \
  > /dev/null 2>&1 </dev/null || fail "run2 failed"
grep -q '^\[TRACE\]' "$out/steps/001-terraform-plan/terraform.log" || fail "the shell's TF_LOG=TRACE was not kept"
pass "the shell's own TF_LOG is kept"

out="$work/run3"
( cd "$work/project" && "$record" --out "$out" --tf-log off -- terraform plan ) \
  > /dev/null 2>&1 </dev/null || fail "run3 failed"
[ ! -e "$out/steps/001-terraform-plan/terraform.log" ] || fail "--tf-log off still wrote terraform.log"
grep -q 'no TF_LOG_PATH' "$out/steps/001-terraform-plan/stderr.log" || fail "--tf-log off still set TF_LOG_PATH"
[ ! -e "$out/steps/001-terraform-plan/fake.log" ] || fail "fake.log was written without --fake-log"
pass "--tf-log off leaves logging alone, and no --fake-log means no fake.log"

# --- --wrap "" and --no-pty ----------------------------------------------------

out="$work/run4"
( cd "$work/project" && "$record" --out "$out" --wrap "" -- terraform plan ) \
  > /dev/null 2>&1 </dev/null || fail "run4 failed"
[ -z "$(ls "$out/steps")" ] || fail "--wrap \"\" still made a step"
[ -z "$(ls "$out/bin")" ] || fail "--wrap \"\" still made a shim"
grep -q 'stub terraform plan' "$out/terminal.txt" || fail "--wrap \"\" lost the transcript"
grep -q 'No wrapped command ran' "$out/index.md" || fail "index.md does not say nothing was wrapped"
pass "--wrap \"\" records the transcript alone"

out="$work/run5"
status=0
( cd "$work/project" && "$record" --out "$out" --no-pty -- ./drive.sh plain ) \
  > "$work/run5.stdout" 2>&1 </dev/null || status=$?
[ "$status" = 3 ] || fail "--no-pty: exited $status, want 3"
grep -q 'driver start plain' "$out/terminal.txt" || fail "--no-pty lost the transcript"
grep -q 'stub warning on stderr for plan' "$out/terminal.txt" || fail "--no-pty lost stderr"
[ -d "$out/steps/001-terraform-plan" ] || fail "--no-pty made no step"
grep -q 'stub terraform plan' "$out/steps/001-terraform-plan/terminal.txt" || fail "--no-pty: the step has no terminal slice"
grep -q '^pty:        no' "$out/command.txt" || fail "command.txt does not say there was no pty"
pass "--no-pty works the same way without a terminal"

# --- run.json, the git probe and --after ---------------------------------------

out="$work/run1"
python3 - "$out/run.json" "$work/project" <<'EOF' || fail "run.json is wrong"
import json, sys
r = json.load(open(sys.argv[1]))
want = {"schema": "k0rdent.record-run/1", "argv": ["./drive.sh", "a b'c"], "cwd": sys.argv[2],
        "exit_code": 3, "finished": True, "pty": True, "tf_log": "DEBUG", "wrapped": ["terraform", "tofu"],
        "steps": 4, "steps_failed": 1, "git": None}
for k, v in want.items():
    assert r.get(k) == v, (k, r.get(k), v)
assert r["versions"]["terraform"].startswith("Terraform v9.9.9-stub, "), r["versions"]
assert r["started"].endswith("Z") and r["ended"] >= r["started"] and r["duration_seconds"] >= 0, r
assert r["fake_log"].endswith("/fake.log"), r["fake_log"]
EOF
pass "run.json describes the run for tools, with git null outside a checkout"

# A walkthrough.sh run folder: the provider worktree beside the terraform.rc
# that TF_CLI_CONFIG_FILE names, and the walkthrough copy, which is no
# checkout, as the current directory.
mkdir -p "$work/runfolder/provider" "$work/runfolder/walkthrough/cluster"
( cd "$work/runfolder/provider" && git init -q && git -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m init )
sha="$(git -C "$work/runfolder/provider" rev-parse HEAD)"
printf 'provider_installation {}\n' > "$work/runfolder/terraform.rc"
out="$work/run7"
( cd "$work/runfolder/walkthrough/cluster" && TF_CLI_CONFIG_FILE="$work/runfolder/terraform.rc" "$record" --out "$out" -- terraform plan ) \
  > /dev/null 2>&1 </dev/null || fail "run7 failed"
[ "$(python3 -c 'import json,sys; g=json.load(open(sys.argv[1]))["git"]; print(g["commit"], g["dirty"], g["dir"])' "$out/run.json")" = "$sha False $work/runfolder/provider" ] \
  || fail "the git probe did not find the provider worktree beside terraform.rc: $(grep -A5 '"git"' "$out/run.json")"
grep -q "^git:        $(printf '%s' "$sha" | cut -c1-12) [^ ]*, in $work/runfolder/provider$" "$out/command.txt" || fail "command.txt lacks the git line: $(grep git: "$out/command.txt")"
pass "the git commit comes from the provider worktree beside TF_CLI_CONFIG_FILE"

cat > "$work/bin/after.sh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "$AFTER_LOG"
[ -f "$2/index.md" ] || { echo "no index.md yet" >> "$AFTER_LOG"; exit 9; }
exit 5
EOF
chmod +x "$work/bin/after.sh"
out="$work/run8"
status=0
( cd "$work/project" && AFTER_LOG="$work/after.log" "$record" --out "$out" --after "after.sh --flag" -- ./drive.sh x ) \
  > /dev/null 2> "$work/run8.stderr" </dev/null || status=$?
[ "$status" = 3 ] || fail "--after changed the exit code to $status"
[ "$(cat "$work/after.log")" = "--flag $out" ] || fail "--after got the wrong arguments: $(cat "$work/after.log")"
grep -q '^record: after.sh exited 5$' "$work/run8.stderr" || fail "--after's exit code was not reported: $(grep record: "$work/run8.stderr")"
grep -q "^record: $out\$" "$work/run8.stderr" || fail "the directory was not named before --after ran"
pass "--after runs with the finished directory, split on spaces, and its exit code is reported"

# --- the arguments --------------------------------------------------------------

status=0
"$record" --out "$work/run1" -- true > /dev/null 2> "$work/err" || status=$?
[ "$status" = 2 ] && grep -q 'already exists' "$work/err" || fail "an existing --out was not refused: $status $(cat "$work/err")"
status=0
"$record" > /dev/null 2> "$work/err" || status=$?
[ "$status" = 2 ] && grep -q '^Usage' "$work/err" || fail "no command did not print the usage: $status"
status=0
"$record" --help > "$work/out" 2>&1 || status=$?
[ "$status" = 0 ] && grep -q '^Usage' "$work/out" || fail "--help did not print the usage and exit 0: $status"
status=0
( cd "$work/project" && "$record" --out "$work/run6" -- no-such-command-here ) > /dev/null 2>&1 </dev/null || status=$?
[ "$status" = 127 ] || fail "a missing command came back as $status, want 127"
pass "an existing directory, no command, --help and a missing command"

echo "record test: PASS"
