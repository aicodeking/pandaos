#!/usr/bin/env python3
"""Closure sizes of the real installable systems, and the size of an update.

usage: closures.py --budgets FILE --out FILE
                   --system NAME=CLOSUREINFO ...
                   [--update NAME=OLD_CLOSUREINFO:NEW_CLOSUREINFO]

CLOSUREINFO is a `pkgs.closureInfo` output: total-nar-size is what
`nix path-info -S` prints, and registration lists every path with its NAR size.
Prints `METRIC {json}`, writes the same object to --out, and exits 1 if a
system's closureMB exceeds budgets[NAME].closureMB.
"""
import argparse
import json
import sys
from pathlib import Path

MB = 1024 * 1024
HASH_PREFIX = len("/nix/store/") + 33  # strip "/nix/store/<hash>-" when printing


def read_registration(closure_info):
    """{store path: NAR size} from a closureInfo registration file.

    Per path: path, narHash, narSize, deriver, nRefs, then nRefs reference lines.
    """
    lines = (Path(closure_info) / "registration").read_text().split("\n")
    sizes = {}
    i = 0
    while i + 4 < len(lines) and lines[i]:
        sizes[lines[i]] = int(lines[i + 2])
        i += 5 + int(lines[i + 4])
    return sizes


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
    args = ap.parse_args(argv)
    budgets = json.loads(Path(args.budgets).read_text())

    metrics = {"test": "closures"}
    failures = []
    for spec in args.system:
        name, ci = spec.split("=", 1)
        total = int((Path(ci) / "total-nar-size").read_text())
        mb = total // MB
        print(f"== {name}: {mb} MB, largest paths:")
        top(read_registration(ci), 30)
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
