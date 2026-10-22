#!/usr/bin/env bash
#
# Fails when a shell block in a tracked Markdown file holds a # comment.
#
# Readers paste these blocks into their own shell, or run them with an
# editor's run button. Interactive zsh, the macOS default, doesn't treat # as
# a comment, so `cd ../cluster   # or ../cluster-own-project` fails with "cd:
# too many arguments", and a line of its own starting with # fails with
# "command not found". Say it in the prose around the block instead.
#
# That includes docs/resources/, which tfplugindocs writes from each
# resource's import.sh, so import.sh holds commands only.
#
# Usage: check-doc-commands.sh [file.md...]   every tracked .md when none given
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

python3 - "$@" <<'PY'
import re, subprocess, sys

paths = sys.argv[1:] or subprocess.run(
    ["git", "ls-files", "-z", "*.md"],
    check=True, capture_output=True, text=True).stdout.split("\0")
found = 0
for path in filter(None, paths):
    fence, shell = None, False
    with open(path, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            line = line.rstrip("\n")
            m = re.match(r"\s*(```+|~~~+)\s*([\w-]*)", line)
            if m:
                if fence is None:
                    fence, shell = m.group(1), m.group(2) in ("sh", "bash", "shell", "zsh")
                elif m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence):
                    fence, shell = None, False
                continue
            if shell and re.search(r"^\s*#|\S\s+#(\s|$)", line):
                found += 1
                print(f"::error file={path},line={n}::a # comment in a shell block breaks when pasted into zsh. Move it into the prose")
                print(f"{path}:{n}: {line}", file=sys.stderr)
sys.exit(1 if found else 0)
PY
echo 'doc commands: PASS'
