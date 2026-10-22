#!/usr/bin/env python3
"""Write internal/apiaudit/data/visibility.json from the pinned contract.

Visibility is the difference between "the platform cannot do this" and "the
platform can do this and a tenant may not ask", and those need completely
different responses from completely different people. The contract records it
per operation as `x-visibility`; nothing downstream records it, because the
generated client is produced for one plane and simply omits the rest.

# Why this parses YAML structure rather than searching text

Two drafts got this wrong before it was right, in ways that both looked
plausible.

`x-visibility` takes two forms, a scalar and a block sequence, and the contract
uses both. Handle only one and the reader falls silently through to whatever it
finds next.

And "next" is the trap. Searching forward from an operationId without bounding
the window runs into the following operation and reports its visibility as this
one's. That produced `createAddressPool: public` and `listAddressPools:
internal`, which is backwards, and it survived a first review because an API
that lets tenants create a pool they cannot list is odd but not impossible.

Bounding the window forward is not enough either, because the description that
also states the visibility sits *before* operationId in some operations and
after it in others. A forward window then reads the next operation's prose and
reports seventeen contradictions that do not exist.

So both keys are located as siblings of operationId in the same mapping, by
indentation. The prose is then a real cross-check: every operation states its
visibility twice, and a genuine disagreement is a contract defect worth
reporting rather than silently resolving.

Each operation's method and path template are recorded too, under "routes".
They are found the same way, by indentation: the key one level out from
operationId is the method, and the one out from that is the path. The
acceptance suite uses them to match every request the provider sent against
the operation it called, so a request to an operator-only operation fails the
run instead of passing unnoticed.

The spec's sha256 and release tag are recorded beside the map, and a test in
internal/apiaudit fails when they no longer match the pinned spec. Re-run
when the contract re-pins:

    scripts/gen-api-visibility.py [path/to/openapi.yaml]

The default is a sibling k0rdent-clients checkout: the full contract, which
names operator-only operations too.
"""
import collections
import hashlib
import json
import os
import re
import sys

here = os.path.dirname(os.path.abspath(__file__))
spec = sys.argv[1] if len(sys.argv) > 1 else os.path.join(
    here, "..", "..", "k0rdent-clients", "go", "spec", "openapi.yaml")
raw = open(spec, "rb").read()
lines = raw.decode("utf-8").split("\n")

release = ""
source_json = os.path.join(os.path.dirname(spec), "SOURCE.json")
if os.path.exists(source_json):
    release = json.load(open(source_json)).get("releaseTag", "")


def indent(s):
    return len(s) - len(s.lstrip(" "))


def sibling_range(i):
    """The lines of the mapping the key at line i belongs to."""
    ind = indent(lines[i])
    lo = i
    while lo > 0 and not (lines[lo - 1].strip() and indent(lines[lo - 1]) < ind):
        lo -= 1
    hi = i
    while hi + 1 < len(lines) and not (lines[hi + 1].strip() and indent(lines[hi + 1]) < ind):
        hi += 1
    return lo, hi


def sibling_key(lo, hi, ind, key):
    for j in range(lo, hi + 1):
        if re.match(r"^\s{%d}%s:" % (ind, re.escape(key)), lines[j]):
            return j
    return None


def enclosing_key(i):
    """The key of the mapping that the line at i sits inside."""
    ind = indent(lines[i])
    for j in range(i - 1, -1, -1):
        if lines[j].strip() and indent(lines[j]) < ind:
            return j, lines[j].strip().rstrip(":")
    return None, None


METHODS = {"get", "put", "post", "delete", "patch", "head", "options", "trace"}
paths_at = lines.index("paths:")
ops, routes, unresolved, disagreed, unrouted = {}, {}, [], [], []
for i, line in enumerate(lines):
    m = re.match(r"^(\s+)operationId:\s*(\w+)\s*$", line)
    if not m:
        continue
    name, ind = m.group(2), indent(line)
    lo, hi = sibling_range(i)

    mj, method = enclosing_key(i)
    pj, path = enclosing_key(mj) if mj is not None else (None, None)
    if method in METHODS and path and path.startswith("/") and pj > paths_at \
            and indent(lines[pj]) == 2:
        routes[name] = {"method": method.upper(), "path": path}
    else:
        unrouted.append(name)

    declared = []
    j = sibling_key(lo, hi, ind, "x-visibility")
    if j is not None:
        rest = lines[j].split(":", 1)[1].strip()
        if rest:
            declared = [rest]
        else:
            k = j + 1
            while k <= hi and lines[k].strip().startswith("- "):
                declared.append(lines[k].strip()[2:].strip())
                k += 1

    stated = []
    j = sibling_key(lo, hi, ind, "description")
    if j is not None:
        block = "\n".join(lines[j:min(j + 12, hi + 1)])
        d = re.search(r"\*\*Visibility:\*\*\s*([^\n]*?)\(x-visibility\)", block)
        if d:
            stated = re.findall(r"`(\w+)`", d.group(1))

    if not declared and not stated:
        unresolved.append(name)
        continue
    if declared and stated and sorted(set(declared)) != sorted(set(stated)):
        disagreed.append("%s: x-visibility says %s, its description says %s"
                         % (name, sorted(set(declared)), sorted(set(stated))))
    ops[name] = sorted(set(declared or stated))

out = os.path.join(here, "..", "internal/apiaudit/data/visibility.json")
with open(out, "w") as f:
    json.dump({
        "source": {"releaseTag": release, "sha256": hashlib.sha256(raw).hexdigest()},
        "operations": ops,
        "routes": routes,
    }, f, indent=1, sort_keys=True)
    f.write("\n")

print("operations: %d, routed: %d" % (len(ops), len(routes)))
for k, n in sorted(collections.Counter(" + ".join(v) for v in ops.values()).items()):
    print("  %-20s %d" % (k, n))
if unrouted:
    print("no method and path found: %s" % ", ".join(unrouted))
if unresolved:
    print("no visibility declared: %s" % ", ".join(unresolved))
if disagreed:
    print("\nthe key and the description disagree, report upstream:")
    for d in disagreed:
        print("  " + d)
