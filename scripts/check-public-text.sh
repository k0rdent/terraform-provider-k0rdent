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
#   check-public-text.sh --commits RANGE --text NAME FILE
#                                   the messages of the commits in RANGE, and
#                                   FILE's text reported as NAME, such as a
#                                   pull request's title. Each repeats.
#
# Exits 0 when clean, 1 on a hit, 2 when it can't run.
set -euo pipefail

start="$PWD"
cd "$(git rev-parse --show-toplevel)"

python3 - "$start" "$@" <<'PY'
import hashlib, os, re, subprocess, sys

START = sys.argv.pop(1)  # where a relative --text FILE is read from

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
# (source, bytes) from --commits and --text, scanned instead of files.
texts = []
commits = 0
text_mode = args[:1] in (["--commits"], ["--text"])
if text_mode:
    while args:
        if args[0] == "--commits" and len(args) > 1:
            log = git("log", "-z", "--format=%H%n%B", "--end-of-options", args[1], "--")
            for entry in filter(None, log.split(b"\0")):
                sha, _, message = entry.partition(b"\n")
                texts.append((f"commit {sha.decode()[:12]}", message))
                commits += 1
            args = args[2:]
        elif args[0] == "--text" and len(args) > 2:
            try:
                with open(os.path.join(START, args[2]), "rb") as f:
                    texts.append((args[1], f.read()))
            except OSError as e:
                die(f"can't read {args[2]}: {e}")
            args = args[3:]
        else:
            die("--commits RANGE and --text NAME FILE go together, and with nothing else")
    paths = []
elif staged:
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


def sources():
    """Each (path or source, bytes) to scan."""
    yield from texts
    for path in paths:
        data = content(path)
        if data is not None and b"\0" not in data[:8000]:  # binary, as grep -I would skip it
            yield path, data


hits = {}
for path, data in sources():
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

def esc(s, prop=False):
    """s escaped for a workflow command, so it can't end the command early."""
    s = s.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
    return s.replace(":", "%3A").replace(",", "%2C") if prop else s


def shown(s):
    """s with control characters escaped, and without the "##[" the Actions
    runner reads as a command anywhere in a line."""
    s = s if s.isprintable() else ascii(s)[1:-1]
    return s.replace("##[", "#\\x23[")


annotate = os.environ.get("GITHUB_ACTIONS") == "true"
total = 0
for (name, pattern), found in hits.items():
    print(f"== {name} ({len(found)} hits) /{pattern}/")
    for path, lineno, tok in found:
        path, tok = shown(path), shown(tok)
        print(f"{path}:{lineno}:{tok}")
        if annotate and text_mode:
            print(f"::error title=public text::{esc(f'rule {name} matched {tok!r} in {path} line {lineno}')}")
        elif annotate:
            print(f"::error file={esc(path, True)},line={lineno}::{esc(f'public text rule {name} matched {tok!r}')}")
    print()
    total += len(found)

if text_mode:
    where = f"{commits} commit message(s) and {len(texts) - commits} text(s)"
else:
    where = f"{len(paths)} {'staged' if staged else 'tracked'} file(s)"
print(f"public text: {total} hit(s) in {where}")
if total:
    print(f"Reword each hit, or add a narrow PATH:MATCH line to {ALLOW} when it is meant to be there.")
sys.exit(1 if total else 0)
PY
