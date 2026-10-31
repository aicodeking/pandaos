#!/usr/bin/env python3
"""Check the input probe's form.log (smoke/input-probe.json).

usage: probecheck.py LOG X1,Y1 X2,Y2 TEXT

Passes when both clicks arrived 1:1 (the second sits at the same offset from
its target as the first, within 2 px, so window placement does not matter),
the wheel moved down and TEXT was typed. Prints what it saw either way.
"""
import sys


def check(lines, p1, p2, text):
    evs = [dict(kv.split("=", 1) for kv in line.strip().split("|") if "=" in kv) for line in lines if line.strip()]
    load = next((e for e in evs if e.get("ev") == "load"), {})
    downs = [(round(float(e["x"])), round(float(e["y"]))) for e in evs if e.get("ev") == "down"]
    seen = f"page {load.get('w')}x{load.get('h')} dpr {load.get('dpr')}, downs {downs}"
    if len(downs) < 2:
        return False, f"want 2 mouse downs; {seen}"
    (ax, ay), (bx, by) = downs[:2]
    off = (ax - p1[0], ay - p1[1])
    if abs(bx - p2[0] - off[0]) > 2 or abs(by - p2[1] - off[1]) > 2:
        return False, f"clicks not 1:1 (offset {off}); {seen}"
    if not any(e.get("ev") == "wheel" and e.get("dy") == "1" for e in evs):
        return False, f"no wheel-down event; {seen}"
    if not any(text in e.get("text", "") for e in evs if e.get("ev") == "keys"):
        typed = [e.get("text") for e in evs if e.get("ev") == "keys"][-1:]
        return False, f"typed {typed}, want {text!r}; {seen}"
    return True, f"ok: offset {off}, wheel, typed {text!r}; {seen}"


def point(s):
    x, y = s.split(",")
    return int(x), int(y)


if __name__ == "__main__":
    try:
        with open(sys.argv[1]) as f:
            lines = f.readlines()
    except FileNotFoundError:
        lines = []
    ok, msg = check(lines, point(sys.argv[2]), point(sys.argv[3]), sys.argv[4])
    print(msg)
    sys.exit(0 if ok else 1)
