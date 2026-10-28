#!/usr/bin/env python3
"""Self-check for collect.py: python3 bench/test_collect.py"""
import json
import os
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import collect  # noqa: E402

with tempfile.TemporaryDirectory() as tmp:
    t = Path(tmp)
    (t / "result-boot-headless").mkdir()
    (t / "result-boot-headless" / "metrics.json").write_text(
        json.dumps({"test": "boot-headless", "profile": "headless", "idleRamMB": 150, "closureMB": 900})
    )
    (t / "slow.log").write_text(
        "vm-test-run-slow-network> booting\n"
        'vm-test-run-slow-network> METRIC {"roundtripSec": 42.5, "test": "slow-network"}\n'
        "vm-test-run-slow-network> METRIC {broken\n"
    )
    results = collect.collect([t / "result-boot-headless", t / "slow.log", t / "missing"])
    assert results["boot-headless"]["idleRamMB"] == 150, results
    assert results["slow-network"]["roundtripSec"] == 42.5, results
    md = collect.table(results, {"headless": {"idleRamMB": 200}})
    assert "| boot-headless | idleRamMB | 150 | 200 |" in md, md
    md = collect.table({"closures": {"test": "closures", "headlessClosureMB": 900, "updateBytes": 5}},
                       {"headless": {"closureMB": 1500}})
    assert "| closures | headlessClosureMB | 900 | 1500 |" in md and "| closures | updateBytes | 5 |  |" in md, md

    os.environ["BENCH_RESULTS_DIR"] = str(t / "out")
    assert collect.main([str(t / "slow.log")]) == 0
    written = json.loads(next((t / "out").glob("*.json")).read_text())
    assert written["results"]["slow-network"]["roundtripSec"] == 42.5, written

print("ok")
