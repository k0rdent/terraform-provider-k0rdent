#!/usr/bin/env bash
#
# Fails when a tracked file holds text that doesn't belong in a public repo:
# ticket keys, internal links and hosts, people, home paths, deal names, AI
# trailers. The rules are .github/public-text/denylist.txt, and hashed.txt for
# names that file shouldn't spell out. Each allowed exception is a line in
# allowlist.txt beside them.
#
# GitHub's secret scanning can't hold custom patterns on a public repo, so
# this is the only gate for these. gitleaks covers secrets.
#
# Usage:
#   check-public-text.sh            every tracked file, as it is on disk
#   check-public-text.sh FILE...    those tracked files only
#   check-public-text.sh --staged   what the next commit adds, from the index
#   check-public-text.sh --hash NAME FLAGS WORD
#                                   the hashed.txt line for WORD
#
# Exits 0 when clean, 1 on a hit, 2 when it can't run.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

python3 - "$@" <<'PY'
import hashlib, os, re, subprocess, sys

DENY = ".github/public-text/denylist.txt"
HASHED = ".github/public-text/hashed.txt"
ALLOW = ".github/public-text/allowlist.txt"


def digest(word, flags):
    return hashlib.sha256((word.lower() if flags == "i" else word).encode()).hexdigest()


if sys.argv[1:2] == ["--hash"]:
    if len(sys.argv) != 5 or sys.argv[3] not in ("i", "-"):
        print("usage: check-public-text.sh --hash NAME i|- WORD", file=sys.stderr)
        sys.exit(2)
    if len(re.split(r"[-.]", sys.argv[4].strip("-."))) > 4:
        print("check-public-text.sh: a hashed name has at most 4 parts split at \"-\" or \".\"", file=sys.stderr)
        sys.exit(2)
    print(f"{sys.argv[2]}:{sys.argv[3]}:{digest(sys.argv[4], sys.argv[3])}")
    sys.exit(0)


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

# hashed[flags][sha256] is the rule a word with that hash breaks.
hashed = {"i": {}, "-": {}}
for n, line in entries(HASHED):
    parts = line.split(":", 2)
    if len(parts) != 3 or parts[1] not in hashed or not re.fullmatch(r"[0-9a-f]{64}", parts[2]):
        die(f"{HASHED}:{n} is not NAME:FLAGS:SHA256")
    hashed[parts[1]][parts[2]] = parts[0]

WORD = re.compile(r"#?[A-Za-z0-9][A-Za-z0-9._-]*")
# The most "-" and "." separated parts a hashed name may have.
MAX_PARTS = 4


def hashed_hits(line):
    """Each (rule, word) a line breaks in hashed.txt. A word is cut at each
    "-" and ".", and every run of whole parts is tried, with and without the
    mark after it: a name inside a longer word, a host under a subdomain and
    a prefix such as "#name-" all match."""
    for m in WORD.finditer(line):
        word = m.group(0).rstrip(".-")
        cuts = [0] + [i + 1 for i, c in enumerate(word) if c in "-."]
        ends = [i for i, c in enumerate(word) if c in "-."] + [len(word)]
        tries = set()
        # A run is at most MAX_PARTS parts, so a long hyphenated or dotted
        # word costs a few tries per part rather than one per pair of parts.
        for n, a in enumerate(cuts):
            for b in ends[n:n + MAX_PARTS]:
                tries.add(word[a:b])
                tries.add(word[a:b + 1])
        for t in tries:
            for flags, table in hashed.items():
                rule = table.get(digest(t, flags))
                if rule:
                    yield rule, t

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
        for name, tok in hashed_hits(line):
            if any(p.search(path) and t.search(tok) for p, t in allows):
                continue
            hits.setdefault((name, "a word in " + HASHED), []).append((path, lineno, tok))

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
