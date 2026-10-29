#!/usr/bin/env python3
"""Closure sizes of the real installable systems, and the size of an update.

usage: closures.py --budgets FILE --out FILE
                   --system NAME=CLOSUREINFO ...
                   [--update NAME=OLD_CLOSUREINFO:NEW_CLOSUREINFO]
                   [--why PREFIX ...]

CLOSUREINFO is a `pkgs.closureInfo` output: total-nar-size is what
`nix path-info -S` prints, and registration lists every path with its NAR size.
Prints `METRIC {json}`, writes the same object to --out, and exits 1 if a
system's closureMB exceeds budgets[NAME].closureMB.

--why PREFIX prints, per system, the shortest reference chain from the
system's root to every path whose name (after the hash) starts with PREFIX,
like `nix why-depends`, so a closure cut can target the actual referrer.
"""
import argparse
import json
import sys
from collections import deque
from pathlib import Path

MB = 1024 * 1024
HASH_PREFIX = len("/nix/store/") + 33  # strip "/nix/store/<hash>-" when printing


def read_graph(closure_info):
    """({store path: NAR size}, {store path: [references]}) from a closureInfo
    registration file.

    Per path: path, narHash, narSize, deriver, nRefs, then nRefs reference lines.
    """
    lines = (Path(closure_info) / "registration").read_text().split("\n")
    sizes, refs = {}, {}
    i = 0
    while i + 4 < len(lines) and lines[i]:
        n = int(lines[i + 4])
        sizes[lines[i]] = int(lines[i + 2])
        refs[lines[i]] = [r for r in lines[i + 5 : i + 5 + n] if r != lines[i]]
        i += 5 + n
    return sizes, refs


def read_registration(closure_info):
    """{store path: NAR size} from a closureInfo registration file."""
    return read_graph(closure_info)[0]


def short(path):
    return path[HASH_PREFIX:]


def why(sizes, refs, prefixes):
    """{matching path: [root, ..., path]} shortest chains from the root(s).

    Roots are paths nothing else in the closure refers to (the toplevel).
    """
    referenced = {r for rs in refs.values() for r in rs}
    roots = sorted(p for p in sizes if p not in referenced)
    parent = {r: None for r in roots}
    queue = deque(roots)
    while queue:
        p = queue.popleft()
        for r in sorted(refs.get(p, [])):
            if r not in parent:
                parent[r] = p
                queue.append(r)
    chains = {}
    for p in sizes:
        if any(short(p).startswith(x) for x in prefixes) and p in parent:
            chain = [p]
            while parent[chain[-1]] is not None:
                chain.append(parent[chain[-1]])
            chains[p] = chain[::-1]
    return chains


def top(sizes, n):
    for path, size in sorted(sizes.items(), key=lambda kv: -kv[1])[:n]:
        print(f"{size // MB:6d} MB  {path[HASH_PREFIX:]}")


def update_size(old, new):
    """Paths a machine on `old` must fetch to switch to `new`, and their NAR bytes."""
    added = {p: s for p, s in new.items() if p not in old}
    return added, sum(added.values())


def main(argv):
    ap = argparse.ArgumentParser()
    ap.add_argument("--budgets", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--system", action="append", default=[], metavar="NAME=CLOSUREINFO")
    ap.add_argument("--update", metavar="NAME=OLD:NEW")
    ap.add_argument("--why", action="append", default=[], metavar="PREFIX")
    args = ap.parse_args(argv)
    budgets = json.loads(Path(args.budgets).read_text())

    metrics = {"test": "closures"}
    failures = []
    for spec in args.system:
        name, ci = spec.split("=", 1)
        total = int((Path(ci) / "total-nar-size").read_text())
        mb = total // MB
        print(f"== {name}: {mb} MB, largest paths:")
        sizes, refs = read_graph(ci)
        top(sizes, 30)
        chains = why(sizes, refs, args.why)
        for path in sorted(chains, key=lambda p: -sizes[p]):
            print(f"== {name} why {sizes[path] // MB} MB {path[HASH_PREFIX:]}:")
            print("     " + " -> ".join(p[HASH_PREFIX:] for p in chains[path]))
        metrics[f"{name}ClosureMB"] = mb
        limit = budgets.get(name, {}).get("closureMB")
        if limit is not None and mb > limit:
            failures.append(f"{name} closure {mb} MB > budget {limit} MB")

    if args.update:
        name, pair = args.update.split("=", 1)
        old_ci, new_ci = pair.split(":", 1)
        added, nbytes = update_size(read_registration(old_ci), read_registration(new_ci))
        print(f"== {name} update: {len(added)} new paths, {nbytes} NAR bytes:")
        top(added, 30)
        metrics["updateBytes"] = nbytes
        metrics["updatePaths"] = len(added)

    print("METRIC " + json.dumps(metrics, sort_keys=True))
    Path(args.out).write_text(json.dumps(metrics, indent=2, sort_keys=True) + "\n")
    for f in failures:
        print(f"FAIL: {f}", file=sys.stderr)
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
