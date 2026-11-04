#!/usr/bin/env bash
#
# Records a run: everything the terminal showed, and each terraform or tofu
# command's own output and logs, filed under the command that produced them.
#
#   scripts/record.sh [options] -- <command> [args...]
#
# For example, in a run folder from walkthrough.sh setup, after . ./setup.sh:
#
#   ~/path/to/provider/scripts/record.sh -- ./walkthrough.sh up cluster
#
# and in a clone:
#
#   scripts/record.sh -- make walkthrough-test
#   scripts/record.sh --fake-log .demo/fake.log -- make demo
#
# The command runs as it would without this script: it reads the same stdin,
# so a prompt still works, and the terminal still shows everything. What it
# adds is a directory, printed at the end, with:
#
#   command.txt       the command, where and when it ran, its exit code,
#                     and the versions of the tools involved
#   environment.txt   the K0RDENT_*, TF_* and RECORD_* variables it ran with,
#                     secret values replaced
#   terminal.log      the terminal, byte for byte, as `script` recorded it
#   terminal.txt      the same with the control codes stripped
#   exit-code         the command's exit code
#   index.md          every wrapped command: what, where, how long, its exit
#                     code, the files below, and its first error lines
#   steps.jsonl       the same, one JSON object per line, for tools
#   run.json          the run for tools: the command, where and when it ran,
#                     its exit code, the git commit and branch it ran from,
#                     and the tool versions
#   steps/NNN-<tool>-<subcommand>/
#     command.txt     argv, working directory, start, end, duration, exit code
#     meta.json       the same, for tools
#     stdout.log      what the command printed to stdout
#     stderr.log      what it printed to stderr
#     output.log      both streams, in the order they arrived
#     terraform.log   what TF_LOG produced: TF_LOG_PATH points here while
#                     the command runs, so the log reaches the file and
#                     not the terminal
#     fake.log        the fake control plane's log lines during this command,
#                     when --fake-log names its log
#     terminal.txt    the terminal during this command
#     crash.log       Terraform's crash log, moved here when it wrote one
#
# Every file under the directory has the values of SECRET, TOKEN, API_KEY,
# PASSWORD and CREDENTIAL variables replaced with [redacted] once the command
# finishes, including terraform.log, where TF_LOG=TRACE holds request bodies.
# The directory is still yours to look through before you share it: the
# transcript holds whatever the run printed.
#
# Options, each with an environment variable that sets the same thing:
#
#   --out DIR        where to write. Default .record/<UTC time>-<command>
#                    under the current directory.                   RECORD_OUT
#   --tf-log LEVEL   TF_LOG for every wrapped command, when the shell has
#                    none set. Default DEBUG. "off" leaves TF_LOG as it is,
#                    and then no terraform.log is written.       RECORD_TF_LOG
#   --fake-log FILE  the fake control plane's log. Default .demo/fake.log
#                    under the current directory or the clone, when there
#                    is one.                                   RECORD_FAKE_LOG
#   --wrap NAMES     the commands to file by invocation, space-separated.
#                    Default "terraform tofu". "" wraps nothing, and the
#                    directory holds the transcript alone.         RECORD_WRAP
#   --no-pty         don't allocate a pseudo-terminal: the transcript then
#                    has stdout and stderr merged and no colour, and a
#                    command that checks for a terminal sees none.
#                                                                RECORD_NO_PTY
#   --after CMD      run CMD with the directory as its argument once the
#                    directory is complete, for a publisher. CMD is split on
#                    spaces. Its exit code is reported, and the run's is
#                    still the one returned.                     RECORD_AFTER
#
# The git commit and branch in run.json come from the current directory's
# checkout, or, when it isn't one, from the provider/ worktree beside the
# terraform.rc that TF_CLI_CONFIG_FILE names, which is how walkthrough.sh
# setup lays out a run folder.
#
# Needs bash, python3 and `script` (util-linux or BSD; --no-pty needs none).
# Works in bash 3.2, so it runs on a Mac as shipped.
#
# How the wrapping works: the directory gets a bin/ with a symlink for each
# wrapped name, pointing at this script, and bin/ goes first on PATH. A
# wrapped command that runs by name reaches the symlink, this script finds
# the real binary further along PATH, runs it with the streams copied into a
# new step directory, and exits with its code. A command that runs by an
# absolute path, or one in a shell PATH doesn't reach, isn't wrapped, and its
# output is in the transcript only.
#
# Exit status: the command's. 2 when the arguments are wrong or the
# directory can't be made.

set -uo pipefail

self="${BASH_SOURCE[0]}"
case "$self" in /*) ;; *) self="$PWD/$self" ;; esac

usage() {
  cat <<'EOF'
Usage: scripts/record.sh [options] -- <command> [args...]

Runs <command> and writes a directory with the terminal transcript and, for
each terraform or tofu command it ran, that command's output and logs.

  --out DIR        where to write (default .record/<UTC time>-<command>)
  --tf-log LEVEL   TF_LOG for wrapped commands when the shell sets none
                   (default DEBUG; "off" leaves it alone)
  --fake-log FILE  the fake control plane's log (default .demo/fake.log)
  --wrap NAMES     commands to file by invocation (default "terraform tofu")
  --no-pty         no pseudo-terminal; stdout and stderr merge
  --after CMD      run CMD with the directory as its argument at the end
  -h, --help       this text

The header of scripts/record.sh describes every file it writes.
EOF
}

die() { printf 'record: %s\n' "$*" >&2; exit 2; }

utc_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# ---------------------------------------------------------------------------
# The shim: this script reached through bin/<name>, for one wrapped command.
# ---------------------------------------------------------------------------

# real_binary <name> prints the first <name> on PATH outside the record's bin.
real_binary() {
  local name="$1" dir
  local IFS=:
  for dir in $PATH; do
    [ -n "$dir" ] || dir=.
    [ "$dir" != "$RECORD_DIR/bin" ] || continue
    if [ -x "$dir/$name" ] && [ ! -d "$dir/$name" ]; then
      printf '%s' "$dir/$name"
      return 0
    fi
  done
  return 1
}

# subcommand <args...> prints the first argument that isn't an option, with
# only letters, digits, dots, underscores and dashes kept.
subcommand() {
  local arg
  for arg in "$@"; do
    case "$arg" in
      -*) continue ;;
      *) printf '%s' "$arg" | tr -c 'A-Za-z0-9._-' '_' | cut -c1-40; return ;;
    esac
  done
  printf 'noargs'
}

# chdir_arg <args...> prints the directory a -chdir=DIR option names, if any.
chdir_arg() {
  local arg
  for arg in "$@"; do
    case "$arg" in
      -chdir=*) printf '%s' "${arg#-chdir=}"; return ;;
    esac
  done
}

# new_step <name> <subcommand> makes the next free steps/NNN-<name>-<sub>
# directory and prints its path. mkdir is atomic, so two commands at once
# never share one.
new_step() {
  local n count dir
  count=$(find "$RECORD_DIR/steps" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')
  n=$((count + 1))
  while :; do
    dir="$RECORD_DIR/steps/$(printf '%03d' "$n")-$1-$2"
    if mkdir "$dir" 2>/dev/null; then
      printf '%s' "$dir"
      return
    fi
    n=$((n + 1))
    [ "$n" -lt 100000 ] || die "could not make a step directory under $RECORD_DIR/steps"
  done
}

file_size() { if [ -f "$1" ]; then wc -c < "$1" | tr -d ' '; else echo 0; fi; }

# write_meta <step> <name> <real> <status> <start iso> <start s> <end s>
# <stdin a tty> <fake start> <fake end> <terminal start> <terminal end>
# <args...> writes meta.json and command.txt for one step.
write_meta() {
  local step="$1" name="$2" real="$3" status="$4" started="$5" start_s="$6" end_s="$7"
  local fake_from="$8" fake_to="$9"
  shift 9
  local term_from="$1" term_to="$2" interrupted="$3"
  shift 3
  STEP="$step" NAME="$name" REAL="$real" STATUS="$status" STARTED="$started" \
    START_S="$start_s" END_S="$end_s" FAKE_FROM="$fake_from" FAKE_TO="$fake_to" \
    TERM_FROM="$term_from" TERM_TO="$term_to" INTERRUPTED="$interrupted" \
    python3 - "$@" <<'PY'
import json, os, shlex, sys
e = os.environ
step = e["STEP"]
argv = [e["NAME"]] + sys.argv[1:]
meta = {
    "step": os.path.basename(step),
    "command": e["NAME"],
    "argv": argv,
    "command_line": " ".join(shlex.quote(a) for a in argv),
    "binary": e["REAL"],
    "cwd": os.getcwd(),
    "started": e["STARTED"],
    "duration_seconds": int(e["END_S"]) - int(e["START_S"]),
    "exit_code": int(e["STATUS"]),
    "interrupted": e["INTERRUPTED"] == "1",
    "tf_log": e.get("TF_LOG", ""),
    "fake_log_bytes": [int(e["FAKE_FROM"]), int(e["FAKE_TO"])] if e["FAKE_TO"] else None,
    "terminal_bytes": [int(e["TERM_FROM"]), int(e["TERM_TO"])] if e["TERM_TO"] else None,
}
with open(os.path.join(step, "meta.json"), "w") as f:
    json.dump(meta, f, indent=2, sort_keys=True)
    f.write("\n")
with open(os.path.join(step, "command.txt"), "w") as f:
    f.write("command:   %s\n" % meta["command_line"])
    f.write("binary:    %s\n" % meta["binary"])
    f.write("cwd:       %s\n" % meta["cwd"])
    f.write("started:   %s\n" % meta["started"])
    f.write("duration:  %ds\n" % meta["duration_seconds"])
    f.write("exit code: %d%s\n" % (meta["exit_code"], " (interrupted)" if meta["interrupted"] else ""))
    if meta["tf_log"]:
        f.write("TF_LOG:    %s, in terraform.log\n" % meta["tf_log"])
PY
}

# redact_dir <dir> replaces the value of every SECRET, TOKEN, API_KEY, PASSWORD
# or CREDENTIAL variable in the environment, eight characters or longer, with
# [redacted] in every regular file under <dir>. The values reach python
# through its environment, never argv.
redact_dir() {
  python3 - "$1" <<'PY'
import os, re, sys
secret_name = re.compile(r"(SECRET|TOKEN|API_KEY|APIKEY|PASSWORD|PASSWD|CREDENTIAL)", re.I)
values = sorted({v.encode() for k, v in os.environ.items()
                 if secret_name.search(k) and len(v) >= 8}, key=len, reverse=True)
if not values:
    sys.exit(0)
for root, _, files in os.walk(sys.argv[1]):
    for name in files:
        path = os.path.join(root, name)
        if os.path.islink(path) or not os.path.isfile(path):
            continue
        with open(path, "rb") as f:
            data = f.read()
        out = data
        for v in values:
            out = out.replace(v, b"[redacted]")
        if out != data:
            with open(path, "wb") as f:
                f.write(out)
PY
}

# slice_file <source> <from byte> <to byte> <target> copies bytes [from, to)
# of <source> into <target>, and writes nothing when the range is empty.
slice_file() {
  local src="$1" from="$2" to="$3" dst="$4"
  [ -f "$src" ] && [ "$to" -gt "$from" ] || return 0
  tail -c "+$((from + 1))" "$src" | head -c "$((to - from))" > "$dst"
}

shim() {
  local name="$1"; shift
  local real step sub status=0 started start_s end_s interrupted=0
  local fake_from=0 fake_to="" term_from=0 term_to="" chdir cwd_for_crash f

  [ -d "$RECORD_DIR" ] || die "RECORD_DIR $RECORD_DIR is gone; cannot record $name"
  real="$(real_binary "$name")" || die "no $name on PATH apart from this shim, so nothing to run"

  sub="$(subcommand "$@")"
  step="$(new_step "$name" "$sub")"
  started="$(utc_now)"
  start_s="$(date +%s)"
  [ -z "${RECORD_FAKE_LOG:-}" ] || fake_from="$(file_size "$RECORD_FAKE_LOG")"
  term_from="$(file_size "$RECORD_DIR/terminal.log")"

  # TF_LOG as the shell has it, or the recorder's default. TF_LOG_PATH is
  # this step's, whatever the shell says, so each command's log is its own.
  if [ -n "${RECORD_TF_LOG:-}" ] && [ "$RECORD_TF_LOG" != off ] && [ -z "${TF_LOG:-}" ]; then
    export TF_LOG="$RECORD_TF_LOG"
  fi
  if [ -n "${TF_LOG:-}" ] || [ -n "${TF_LOG_CORE:-}" ] || [ -n "${TF_LOG_PROVIDER:-}" ]; then
    export TF_LOG_PATH="$step/terraform.log"
  fi

  # The command's stdout and stderr each go through their own tee, to the
  # terminal and to a file, and both append to output.log. Two writers with
  # O_APPEND keep each write whole, so output.log has the lines in the order
  # they arrived, to within a write. Named pipes rather than process
  # substitution, so bash 3.2 can wait for the tees before reading the files.
  : > "$step/stdout.log"; : > "$step/stderr.log"; : > "$step/output.log"
  mkfifo "$step/.stdout" "$step/.stderr" || die "could not make pipes in $step"
  tee -a "$step/stdout.log" "$step/output.log" < "$step/.stdout" &
  local out_pid=$!
  tee -a "$step/stderr.log" "$step/output.log" < "$step/.stderr" >&2 &
  local err_pid=$!

  # Ctrl-C reaches the command and this shell alike. The command handles it;
  # this shell stays to record the exit code it produces.
  trap 'interrupted=1' INT TERM
  "$real" "$@" > "$step/.stdout" 2> "$step/.stderr"
  status=$?
  trap - INT TERM
  wait "$out_pid" "$err_pid" 2>/dev/null
  rm -f "$step/.stdout" "$step/.stderr"

  end_s="$(date +%s)"
  [ -z "${RECORD_FAKE_LOG:-}" ] || fake_to="$(file_size "$RECORD_FAKE_LOG")"
  term_to="$(file_size "$RECORD_DIR/terminal.log")"

  if [ -n "${RECORD_FAKE_LOG:-}" ]; then
    slice_file "$RECORD_FAKE_LOG" "$fake_from" "$fake_to" "$step/fake.log"
  fi

  # Terraform writes crash.log where it runs, which -chdir moves.
  chdir="$(chdir_arg "$@")"
  cwd_for_crash="${chdir:-.}"
  for f in "$cwd_for_crash"/crash.log "$cwd_for_crash"/crash.*.log; do
    [ -f "$f" ] && mv "$f" "$step/" 2>/dev/null
  done

  write_meta "$step" "$name" "$real" "$status" "$started" "$start_s" "$end_s" \
    "$fake_from" "$fake_to" "$term_from" "$term_to" "$interrupted" "$@"
  [ -z "${TF_LOG_PATH:-}" ] || [ -s "$TF_LOG_PATH" ] || rm -f "$TF_LOG_PATH"
  redact_dir "$step"
  exit "$status"
}

# ---------------------------------------------------------------------------
# The inner runner: the command itself, under `script`. It reads the argv the
# recorder saved, because `script` on Linux takes the command as one string
# for a shell, which an argument with a quote or a space would break.
# ---------------------------------------------------------------------------

inner() {
  local args=() arg status
  while IFS= read -r -d '' arg; do
    args+=("$arg")
  done < "$RECORD_DIR/.command"
  "${args[@]}"
  status=$?
  printf '%s\n' "$status" > "$RECORD_DIR/exit-code"
  exit "$status"
}

# ---------------------------------------------------------------------------
# The recorder.
# ---------------------------------------------------------------------------

# write_environment writes environment.txt: the variables the provider,
# Terraform and this script read, with every secret-named value replaced.
write_environment() {
  python3 - > "$RECORD_DIR/environment.txt" <<'PY'
import os, re
secret_name = re.compile(r"(SECRET|TOKEN|API_KEY|APIKEY|PASSWORD|PASSWD|CREDENTIAL)", re.I)
for k in sorted(os.environ):
    if not re.match(r"(K0RDENT_|TF_|RECORD_|FAKE_|PATH$|SHELL$|TERM$)", k):
        continue
    v = "[redacted]" if secret_name.search(k) else os.environ[k]
    print("%s=%s" % (k, v))
PY
}

# git_probe sets GIT_DIR_USED, GIT_COMMIT, GIT_BRANCH and GIT_DIRTY from the
# current directory's checkout, else from the provider/ worktree beside the
# terraform.rc TF_CLI_CONFIG_FILE names, else leaves them empty.
git_probe() {
  local d
  GIT_DIR_USED="" GIT_COMMIT="" GIT_BRANCH="" GIT_DIRTY=""
  command -v git >/dev/null 2>&1 || return 0
  for d in "$PWD" "${TF_CLI_CONFIG_FILE:+$(dirname "$TF_CLI_CONFIG_FILE")/provider}"; do
    [ -n "$d" ] && [ -d "$d" ] || continue
    GIT_DIR_USED="$(git -C "$d" rev-parse --show-toplevel 2>/dev/null)" || continue
    GIT_COMMIT="$(git -C "$d" rev-parse HEAD 2>/dev/null)" || GIT_COMMIT=""
    GIT_BRANCH="$(git -C "$d" rev-parse --abbrev-ref HEAD 2>/dev/null)" || GIT_BRANCH=""
    if [ -z "$(git -C "$d" status --porcelain 2>/dev/null)" ]; then GIT_DIRTY=no; else GIT_DIRTY=yes; fi
    return 0
  done
}

# tool_versions sets TOOL_VERSIONS, one "name=first line of `name version`"
# per line for each wrapped command found, then go.
tool_versions() {
  local name real version
  TOOL_VERSIONS=""
  for name in $RECORD_WRAP; do
    real="$(real_binary "$name" 2>/dev/null)" || continue
    version="$("$real" version 2>/dev/null)" || version=""
    TOOL_VERSIONS="$TOOL_VERSIONS$name=${version%%
*}${version:+, }$real
"
  done
  if command -v go >/dev/null 2>&1; then
    TOOL_VERSIONS="${TOOL_VERSIONS}go=$(go version 2>/dev/null)
"
  fi
}

# write_command writes command.txt: the command, where and when it runs, and
# what it runs with. The exit code and end time are appended afterwards.
write_command() {
  local line
  {
    printf 'command:    '
    local a
    for a in "${COMMAND[@]}"; do printf "'%s' " "$a"; done
    printf '\ncwd:        %s\n' "$PWD"
    printf 'started:    %s\n' "$STARTED_AT"
    printf 'recorder:   %s\n' "$self"
    printf 'pty:        %s\n' "$([ "$NO_PTY" = 1 ] && echo no || echo "yes ($SCRIPT_FLAVOUR script)")"
    printf 'TF_LOG:     %s\n' "${TF_LOG:-${RECORD_TF_LOG:-off}}${TF_LOG:+ (from the shell)}"
    printf 'fake log:   %s\n' "${RECORD_FAKE_LOG:-none}"
    printf 'wrapped:    %s\n' "${RECORD_WRAP:-nothing}"
    printf 'host:       %s\n' "$(uname -srm 2>/dev/null || echo unknown)"
    if [ -n "$GIT_COMMIT" ]; then
      printf 'git:        %s %s%s, in %s\n' "$(printf '%s' "$GIT_COMMIT" | cut -c1-12)" "$GIT_BRANCH" \
        "$([ "$GIT_DIRTY" = yes ] && echo ' (uncommitted changes)')" "$GIT_DIR_USED"
    fi
    printf '%s' "$TOOL_VERSIONS" | while IFS= read -r line; do
      printf '%-11s %s\n' "${line%%=*}:" "${line#*=}"
    done
  } > "$RECORD_DIR/command.txt"
}

# finalize reads the steps back, slices the transcript per step, strips the
# control codes, redacts and writes index.md, steps.jsonl and run.json.
# The command's argv follows the directory in its arguments.
finalize() {
  local status="$1" ended finished=yes
  shift
  ended="$(utc_now)"
  printf 'ended:      %s\nexit code:  %s\n' "$ended" "$status" >> "$RECORD_DIR/command.txt"
  [ "$EXIT_CODE_SEEN" = yes ] || finished=no
  redact_dir "$RECORD_DIR"
  RUN_STATUS="$status" RUN_STARTED="$STARTED_AT" RUN_ENDED="$ended" RUN_FINISHED="$finished" \
    RUN_PTY="$([ "$NO_PTY" = 1 ] && echo no || echo yes)" RUN_TF_LOG="${TF_LOG:-${RECORD_TF_LOG:-}}" \
    RUN_FAKE_LOG="${RECORD_FAKE_LOG:-}" RUN_WRAP="$RECORD_WRAP" RUN_HOST="$(uname -srm 2>/dev/null || echo unknown)" \
    RUN_GIT_DIR="$GIT_DIR_USED" RUN_GIT_COMMIT="$GIT_COMMIT" RUN_GIT_BRANCH="$GIT_BRANCH" RUN_GIT_DIRTY="$GIT_DIRTY" \
    RUN_VERSIONS="$TOOL_VERSIONS" RUN_RECORDER="$self" \
    python3 - "$RECORD_DIR" "$@" <<'PY'
import glob, json, os, re, shlex, sys
out = sys.argv[1]
argv = sys.argv[2:]
e = os.environ

ansi = re.compile(rb"\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[@-Z\\-_]")

def plain(data):
    data = ansi.sub(b"", data)
    data = data.replace(b"\r\n", b"\n").replace(b"\r", b"\n")
    data = re.sub(rb"[\x00-\x08\x0b\x0c\x0e-\x1f]", b"", data)
    # util-linux script writes these into the file even with -q.
    return re.sub(rb"^Script (started|done) on .*\n?", b"", data, flags=re.M)

terminal = b""
path = os.path.join(out, "terminal.log")
if os.path.exists(path):
    with open(path, "rb") as f:
        terminal = f.read()
    with open(os.path.join(out, "terminal.txt"), "wb") as f:
        f.write(plain(terminal))

steps = []
for meta_path in sorted(glob.glob(os.path.join(out, "steps", "*", "meta.json"))):
    with open(meta_path) as f:
        meta = json.load(f)
    step_dir = os.path.dirname(meta_path)
    rng = meta.get("terminal_bytes")
    if terminal and rng and rng[1] > rng[0]:
        with open(os.path.join(step_dir, "terminal.txt"), "wb") as f:
            f.write(plain(terminal[rng[0]:rng[1]]))
    files = {}
    for name in sorted(os.listdir(step_dir)):
        p = os.path.join(step_dir, name)
        if os.path.isfile(p) and name not in ("meta.json", "command.txt"):
            files[name] = os.path.getsize(p)
    meta["files"] = files
    errors = []
    err_path = os.path.join(step_dir, "stderr.log")
    if os.path.exists(err_path):
        with open(err_path, "rb") as f:
            for line in plain(f.read()).decode("utf-8", "replace").splitlines():
                if re.match(r"\s*(│\s*)?(Error|Warning|panic)\b", line) or "Error:" in line:
                    errors.append(line.strip())
    meta["error_lines"] = errors[:8]
    steps.append(meta)

with open(os.path.join(out, "steps.jsonl"), "w") as f:
    for s in steps:
        f.write(json.dumps(s, sort_keys=True) + "\n")

versions = {}
for line in e["RUN_VERSIONS"].splitlines():
    if "=" in line:
        k, v = line.split("=", 1)
        versions[k] = v
git = None
if e["RUN_GIT_COMMIT"]:
    git = {"dir": e["RUN_GIT_DIR"], "commit": e["RUN_GIT_COMMIT"], "branch": e["RUN_GIT_BRANCH"],
           "dirty": e["RUN_GIT_DIRTY"] == "yes"}
def parse_time(t):
    import calendar, time
    return calendar.timegm(time.strptime(t, "%Y-%m-%dT%H:%M:%SZ"))
run = {
    "schema": "k0rdent.record-run/1",
    "recorder": e["RUN_RECORDER"],
    "argv": argv,
    "command_line": " ".join(shlex.quote(a) for a in argv),
    "cwd": os.getcwd(),
    "started": e["RUN_STARTED"],
    "ended": e["RUN_ENDED"],
    "duration_seconds": max(0, parse_time(e["RUN_ENDED"]) - parse_time(e["RUN_STARTED"])),
    "exit_code": int(e["RUN_STATUS"]),
    "finished": e["RUN_FINISHED"] == "yes",
    "pty": e["RUN_PTY"] == "yes",
    "tf_log": e["RUN_TF_LOG"] if e["RUN_TF_LOG"] not in ("", "off") else None,
    "fake_log": e["RUN_FAKE_LOG"] or None,
    "wrapped": e["RUN_WRAP"].split(),
    "host": e["RUN_HOST"],
    "git": git,
    "versions": versions,
    "steps": len(steps),
    "steps_failed": len([s for s in steps if s["exit_code"] != 0]),
}
with open(os.path.join(out, "run.json"), "w") as f:
    json.dump(run, f, indent=2, sort_keys=True)
    f.write("\n")

with open(os.path.join(out, "command.txt")) as f:
    command = f.read()
exit_code = "?"
try:
    with open(os.path.join(out, "exit-code")) as f:
        exit_code = f.read().strip()
except OSError:
    pass

lines = ["# Recorded run", ""]
lines += ["```text", command.rstrip(), "```", ""]
lines += ["Files: `terminal.txt` is the whole terminal, `terminal.log` the raw recording, "
          "`environment.txt` the variables, `run.json` and `steps.jsonl` the run and this index for tools.", ""]
failed = [s for s in steps if s["exit_code"] != 0]
lines += ["## Steps", ""]
if not steps:
    lines += ["No wrapped command ran. The transcript holds everything.", ""]
else:
    lines += ["%d wrapped command%s, %d with a non-zero exit code." % (
        len(steps), "" if len(steps) == 1 else "s", len(failed)), ""]
    for s in steps:
        mark = "ok" if s["exit_code"] == 0 else "exit %d" % s["exit_code"]
        if s.get("interrupted"):
            mark += ", interrupted"
        lines.append("- `%s`: `%s`, %ds, %s" % (s["step"], s["command_line"], s["duration_seconds"], mark))
    lines.append("")
    for s in steps:
        lines += ["### %s" % s["step"], ""]
        lines += ["- command: `%s`" % s["command_line"]]
        lines += ["- cwd: `%s`" % s["cwd"]]
        lines += ["- started: %s, took %ds, exit code %d%s" % (
            s["started"], s["duration_seconds"], s["exit_code"],
            " (interrupted)" if s.get("interrupted") else "")]
        if s.get("tf_log"):
            lines += ["- TF_LOG=%s, in `terraform.log`" % s["tf_log"]]
        if s["files"]:
            lines += ["- files: " + ", ".join("`%s` (%d bytes)" % (n, b) for n, b in s["files"].items())]
        if s["error_lines"]:
            lines += ["- stderr:", ""]
            lines += ["  ```text"] + ["  " + l for l in s["error_lines"]] + ["  ```"]
        lines.append("")
with open(os.path.join(out, "index.md"), "w") as f:
    f.write("\n".join(lines).rstrip() + "\n")
print("recorded %d wrapped command%s, %d failed, exit code %s" % (
    len(steps), "" if len(steps) == 1 else "s", len(failed), exit_code))
PY
}

main() {
  local out="${RECORD_OUT:-}" tf_log="${RECORD_TF_LOG-DEBUG}" fake_log="${RECORD_FAKE_LOG:-}"
  local wrap="${RECORD_WRAP-terraform tofu}" no_pty="${RECORD_NO_PTY:-0}" after="${RECORD_AFTER:-}"

  while [ $# -gt 0 ]; do
    case "$1" in
      --out) [ $# -ge 2 ] || { usage >&2; exit 2; }; out="$2"; shift 2 ;;
      --out=*) out="${1#--out=}"; shift ;;
      --tf-log) [ $# -ge 2 ] || { usage >&2; exit 2; }; tf_log="$2"; shift 2 ;;
      --tf-log=*) tf_log="${1#--tf-log=}"; shift ;;
      --fake-log) [ $# -ge 2 ] || { usage >&2; exit 2; }; fake_log="$2"; shift 2 ;;
      --fake-log=*) fake_log="${1#--fake-log=}"; shift ;;
      --wrap) [ $# -ge 2 ] || { usage >&2; exit 2; }; wrap="$2"; shift 2 ;;
      --wrap=*) wrap="${1#--wrap=}"; shift ;;
      --no-pty) no_pty=1; shift ;;
      --after) [ $# -ge 2 ] || { usage >&2; exit 2; }; after="$2"; shift 2 ;;
      --after=*) after="${1#--after=}"; shift ;;
      -h | --help) usage; exit 0 ;;
      --) shift; break ;;
      -*) printf 'record: unknown option %s\n' "$1" >&2; usage >&2; exit 2 ;;
      *) break ;;
    esac
  done
  [ $# -gt 0 ] || { usage >&2; exit 2; }
  COMMAND=("$@")

  if [ -z "$fake_log" ]; then
    local top
    if [ -f .demo/fake.log ]; then
      fake_log="$PWD/.demo/fake.log"
    elif top="$(git rev-parse --show-toplevel 2>/dev/null)" && [ -f "$top/.demo/fake.log" ]; then
      fake_log="$top/.demo/fake.log"
    fi
  fi
  if [ -n "$fake_log" ]; then
    case "$fake_log" in /*) ;; *) fake_log="$PWD/$fake_log" ;; esac
  fi

  if [ -z "$out" ]; then
    out=".record/$(date -u +%Y-%m-%d-%H%M%S)-$(basename "$1" | tr -c 'A-Za-z0-9._\n' '-')"
  fi
  case "$out" in /*) ;; *) out="$PWD/$out" ;; esac
  [ ! -e "$out" ] || die "$out already exists. Name another with --out, or wait a second."
  (umask 077 && mkdir -p "$out/steps" "$out/bin") || die "could not make $out"
  chmod 700 "$out" || die "could not make $out private"

  local name
  for name in $wrap; do
    ln -s "$self" "$out/bin/$name" || die "could not make the $name shim in $out/bin"
  done

  export RECORD_DIR="$out"
  export RECORD_TF_LOG="$tf_log"
  export RECORD_WRAP="$wrap"
  if [ -n "$fake_log" ]; then export RECORD_FAKE_LOG="$fake_log"; else unset RECORD_FAKE_LOG; fi
  [ -z "$wrap" ] || export PATH="$out/bin:$PATH"

  NO_PTY="$no_pty"
  SCRIPT_FLAVOUR=""
  if [ "$NO_PTY" != 1 ]; then
    if ! command -v script >/dev/null 2>&1; then
      printf 'record: no `script` on PATH, so no pseudo-terminal: stdout and stderr merge in the transcript\n' >&2
      NO_PTY=1
    elif script --version 2>/dev/null | grep -q util-linux; then
      SCRIPT_FLAVOUR=util-linux
    else
      SCRIPT_FLAVOUR=bsd
    fi
  fi

  STARTED_AT="$(utc_now)"
  git_probe
  tool_versions
  write_environment
  write_command

  # The command's argv, for inner, NUL-separated so any argument survives.
  printf '%s\0' "${COMMAND[@]}" > "$out/.command"

  # -f on util-linux and -F on a Mac flush the file on every write, so the
  # size a shim reads mid-run is where the terminal really is.
  # Ctrl-C in a pseudo-terminal goes to the command, not here. Without one it
  # reaches this shell too, which stays to finish the directory.
  trap ':' INT TERM
  local status
  case "$NO_PTY:$SCRIPT_FLAVOUR" in
    1:*)
      "$self" --inner 2>&1 | tee "$out/terminal.log" ;;
    *:util-linux)
      script -q -f -c "$(printf '%q' "$self") --inner" "$out/terminal.log" ;;
    *)
      if [ "$(uname -s 2>/dev/null)" = Darwin ]; then
        script -q -F "$out/terminal.log" "$self" --inner
      else
        script -q "$out/terminal.log" "$self" --inner
      fi ;;
  esac
  trap - INT TERM
  EXIT_CODE_SEEN=yes
  if [ -s "$out/exit-code" ]; then
    status="$(cat "$out/exit-code")"
  else
    status=1
    EXIT_CODE_SEEN=no
    printf '%s\n' "$status" > "$out/exit-code"
    printf 'record: the command left no exit code, so it did not finish; recorded as 1\n' >&2
  fi
  rm -f "$out/.command"

  local summary
  summary="$(finalize "$status" "${COMMAND[@]}")"
  printf '\nrecord: %s\nrecord: %s\n' "$summary" "$out" >&2

  # The publisher, or whatever --after names, gets the finished directory.
  # It's split on spaces so it can carry options. Its exit code is said and
  # the run's is the one returned.
  if [ -n "$after" ]; then
    local after_status=0
    printf 'record: running %s %s\n' "$after" "$out" >&2
    # shellcheck disable=SC2086 # split on purpose, see above
    $after "$out" || after_status=$?
    [ "$after_status" = 0 ] || printf 'record: %s exited %s\n' "${after%% *}" "$after_status" >&2
  fi
  exit "$status"
}

# Which role this invocation has: a shim when reached through a symlink
# named for a wrapped command, inner under `script`, the recorder otherwise.
case "$(basename "$0")" in
  record.sh)
    if [ "${1:-}" = --inner ] && [ -n "${RECORD_DIR:-}" ]; then
      inner
    else
      main "$@"
    fi
    ;;
  *)
    [ -n "${RECORD_DIR:-}" ] || die "$(basename "$0") is a record.sh shim but RECORD_DIR is not set"
    shim "$(basename "$0")" "$@"
    ;;
esac
