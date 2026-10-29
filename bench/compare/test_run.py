#!/usr/bin/env python3
"""Self-test for run.py's parsing and table (no VMs): python3 bench/compare/test_run.py"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run  # noqa: E402

MEMINFO = """MemTotal:        2000000 kB
MemFree:         1000000 kB
MemAvailable:    1500000 kB
Buffers:           10240 kB
Cached:           409600 kB
SwapCached:            0 kB
Shmem:             20480 kB
SReclaimable:      51200 kB
"""
# used = 2000000-1000000-10240-409600-51200+20480 = 549440 kB -> 536 MB
assert run.meminfo_mb(MEMINFO) == (536, 488), run.meminfo_mb(MEMINFO)
assert run.parse_kv("os=Debian GNU/Linux 13 (trixie)\nprocs=12\nnoise\n") == {
    "os": "Debian GNU/Linux 13 (trixie)", "procs": "12"}
md = run.table([
    {"name": "A", "ok": True, "ramUsedShmemMB": 1, "procs": 90, "userProcs": 20},
    {"name": "B", "ok": False, "error": "no ssh"},
])
assert "| A | 1 | ? | 90 (20) |" in md, md
assert md.splitlines()[3].count("|") == md.splitlines()[0].count("|"), md
print("ok")
