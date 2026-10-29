#!/usr/bin/env python3
"""Merge PandaOS VM-test metrics into bench/results/<date>.json and print a markdown table.

usage: collect.py PATH...
  PATH is a check output dir (reads its metrics.json), a .json file, or a build
  log containing `METRIC {json}` lines. Later sources for the same test merge
  over earlier ones. Set BENCH_RESULTS_DIR to write somewhere else.
"""
import datetime
import json
import os
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
BUDGETS = HERE.parent / "tests" / "budgets.json"
MARKER = "METRIC "


def warn(msg):
    print(f"collect.py: {msg}", file=sys.stderr)


def parse_log(text):
    # raw_decode stops at the end of the JSON object, so console output glued
    # onto the end of a METRIC line (interleaved writes) does not lose it.
    decoder = json.JSONDecoder()
    for line in text.splitlines():
        i = line.find(MARKER + "{")
        if i < 0:
            continue
        try:
            yield decoder.raw_decode(line[i + len(MARKER):])[0]
        except json.JSONDecodeError as e:
            warn(f"bad METRIC line ({e}): {line.strip()}")


def load(path):
    p = Path(path)
    if p.is_dir():
        p = p / "metrics.json"
    if not p.is_file():
        warn(f"skipping {path}: no metrics")
        return []
    if p.suffix == ".json":
        try:
            return [json.loads(p.read_text())]
        except json.JSONDecodeError as e:
            warn(f"skipping {p}: {e}")
            return []
    return list(parse_log(p.read_text(errors="replace")))


def collect(paths):
    results = {}
    for path in paths:
        for m in load(path):
            if not isinstance(m, dict) or not isinstance(m.get("test"), str):
                warn(f"ignoring metric without a test name from {path}: {m!r}")
                continue
            results[m["test"]] = {**results.get(m["test"], {}), **m}
    return results


def budget_for(key, profile, budgets):
    """Budget for a metric: by the test's profile, or by a profile-prefixed key
    (closures reports headlessClosureMB against budgets.headless.closureMB)."""
    if key in budgets.get(profile, {}):
        return budgets[profile][key]
    for p, b in budgets.items():
        rest = key[len(p):]
        if key.startswith(p) and rest:
            sub = rest[0].lower() + rest[1:]
            if sub in b:
                return b[sub]
    return ""


def table(results, budgets):
    rows = ["| test | metric | value | budget |", "|---|---|---|---|"]
    for name in sorted(results):
        m = results[name]
        for key in sorted(k for k in m if k not in ("test", "profile")):
            rows.append(f"| {name} | {key} | {m[key]} | {budget_for(key, m.get('profile'), budgets)} |")
    return "\n".join(rows)


def main(argv):
    if not argv:
        print(__doc__, file=sys.stderr)
        return 2
    results = collect(argv)
    if not results:
        warn("no metrics found")
        return 1
    budgets = json.loads(BUDGETS.read_text()) if BUDGETS.is_file() else {}
    today = datetime.datetime.now(datetime.timezone.utc).date().isoformat()
    out_dir = Path(os.environ.get("BENCH_RESULTS_DIR", HERE / "results"))
    out_dir.mkdir(parents=True, exist_ok=True)
    out = out_dir / f"{today}.json"
    report = {
        "date": today,
        "commit": os.environ.get("GITHUB_SHA"),
        "budgets": budgets,
        "results": results,
    }
    out.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    print(table(results, budgets))
    warn(f"wrote {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
