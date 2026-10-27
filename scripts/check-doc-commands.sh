#!/usr/bin/env bash
#
# Fails when a shell block in a tracked Markdown file holds a # comment.
#
# Interactive zsh, the macOS default, doesn't treat # as a comment, so a pasted
# `cd ../cluster   # or ../cluster-own-project` fails with "cd: too many
# arguments". docs/resources/ counts too: tfplugindocs writes it from each
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
    with open(path, encoding="utf-8") as page:
        for line_no, line in enumerate(page, 1):
            line = line.rstrip("\n")

            fence_match = re.match(r"\s*(```+|~~~+)\s*([\w-]*)", line)
            if fence_match:
                if fence is None:
                    fence, shell = fence_match.group(1), fence_match.group(2) in ("sh", "bash", "shell", "zsh")
                elif fence_match.group(1)[0] == fence[0] and len(fence_match.group(1)) >= len(fence):
                    fence, shell = None, False
                continue

            if shell and re.search(r"^\s*#|\S\s+#(\s|$)", line):
                found += 1
                print(f"::error file={path},line={line_no}::a # comment in a shell block breaks when pasted into zsh. Move it into the prose")
                print(f"{path}:{line_no}: {line}", file=sys.stderr)

sys.exit(1 if found else 0)
PY
echo 'doc commands: PASS'
