#!/usr/bin/env bash
#
# Fails when a tracked file holds text that doesn't belong in a public repo:
# ticket keys, internal links and hosts, people, home paths, deal names, AI
# trailers. The rules are .github/public-text/denylist.txt, and each allowed
# exception is a line in allowlist.txt beside it.
#
# GitHub's secret scanning can't hold custom patterns on a public repo, so
# this is the only gate for these. gitleaks covers secrets.
#
# Usage:
#   check-public-text.sh            every tracked file, as it is on disk
#   check-public-text.sh FILE...    those tracked files only
#   check-public-text.sh --staged   what the next commit adds, from the index
#
# Exits 0 when clean, 1 on a hit, 2 when it can't run.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

python3 - "$@" <<'PY'
import os, re, subprocess, sys

DENY = ".github/public-text/denylist.txt"
ALLOW = ".github/public-text/allowlist.txt"


def die(msg):
    print(f"public text: {msg}", file=sys.stderr)
    sys.exit(2)


def entries(path):
    try:
        with open(path, encoding="utf-8") as f:
            for n, line in enumerate(f, 1):
                line = line.rstrip("\n")
                if line.strip() and not line.lstrip().startswith("#"):
                    yield n, line
    except OSError as e:
        die(f"can't read {path}: {e}")


def compile_or_die(where, pattern, flags=0):
    try:
        return re.compile(pattern, flags)
    except re.error as e:
        die(f"{where}: {pattern!r} is not a valid regular expression: {e}")


rules = []
for n, line in entries(DENY):
    parts = line.split(":", 2)
    if len(parts) != 3 or parts[1] not in ("i", "-") or not parts[2]:
        die(f"{DENY}:{n} is not NAME:FLAGS:REGEX")
    name, flags, pattern = parts
    rules.append((name, pattern, compile_or_die(f"{DENY}:{n}", pattern, re.I if flags == "i" else 0)))

allows = []
for n, line in entries(ALLOW):
    path_re, sep, match_re = line.partition(":")
    if not sep or not path_re or not match_re:
        die(f"{ALLOW}:{n} is not PATH_REGEX:MATCH_REGEX")
    allows.append((compile_or_die(f"{ALLOW}:{n}", path_re), compile_or_die(f"{ALLOW}:{n}", match_re)))


def git(*args):
    r = subprocess.run(["git", *args], capture_output=True)
    if r.returncode != 0:
        die(f"git {' '.join(args)} failed: {r.stderr.decode(errors='replace').strip()}")
    return r.stdout


args = sys.argv[1:]
staged = args[:1] == ["--staged"]
if staged:
    args = args[1:]
    if args:
        die("--staged takes no files")
    paths = git("diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z").decode().split("\0")
else:
    paths = git("ls-files", "-z", "--", *args).decode().split("\0")
    if args and not any(paths):
        die(f"none of {' '.join(args)} is tracked")
paths = [p for p in paths if p]


def content(path):
    if staged:
        return git("show", f":{path}")
    if not os.path.isfile(path):
        return None  # deleted in the working tree, or a submodule
    with open(path, "rb") as f:
        return f.read()


hits = {}
for path in paths:
    data = content(path)
    if data is None or b"\0" in data[:8000]:
        continue  # binary, as grep -I would skip it
    text = data.decode("utf-8", errors="replace")
    for lineno, line in enumerate(text.splitlines(), 1):
        for name, pattern, rx in rules:
            for m in rx.finditer(line):
                tok = m.group(0)
                if any(p.search(path) and t.search(tok) for p, t in allows):
                    continue
                hits.setdefault((name, pattern), []).append((path, lineno, tok))

annotate = os.environ.get("GITHUB_ACTIONS") == "true"
total = 0
for (name, pattern), found in hits.items():
    print(f"== {name} ({len(found)} hits) /{pattern}/")
    for path, lineno, tok in found:
        print(f"{path}:{lineno}:{tok}")
        if annotate:
            print(f"::error file={path},line={lineno}::public text rule {name} matched {tok!r}")
    print()
    total += len(found)

where = "staged file(s)" if staged else "tracked file(s)"
print(f"public text: {total} hit(s) in {len(paths)} {where}")
if total:
    print(f"Reword each hit, or add a narrow PATH:MATCH line to {ALLOW} when it is meant to be there.")
sys.exit(1 if total else 0)
PY
