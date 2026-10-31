#!/usr/bin/env python3
"""The AT-SPI baseline for `--tools atspi`, the same on every OS.

  cu_atspi.py cu_tree|cu_find|cu_act 'JSON-ARGS'   prints one JSON value
  cu_atspi.py do 'JSON-ARGS'                        oracle helper: find + act

cu_tree, cu_find and cu_act take panda-cu's argument schemas and give its
results (components/computer-use/a11y.go), because they run the same AT-SPI
walker: panda-cu-atspi where installed (PandaOS), else a copy of
components/computer-use/atspi.py uploaded next to this file and run with the
system python3 + gi. Errors: exit 1 with the message on stderr.
"""
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SNAP = os.path.join(os.environ.get("XDG_RUNTIME_DIR") or "/tmp", "cubench-atspi-snap.json")
ALIASES = {"button": "push button"}


def helper(*argv, timeout=30):
    exe = shutil.which("panda-cu-atspi")
    cmd = [exe] if exe else [sys.executable, os.path.join(HERE, "atspi.py")]
    r = subprocess.run(cmd + [str(a) for a in argv], capture_output=True, text=True, timeout=timeout)
    try:
        res = json.loads(r.stdout or "{}")
    except json.JSONDecodeError:
        raise RuntimeError(f"AT-SPI helper printed non-JSON: {(r.stdout or r.stderr)[:500]}") from None
    if isinstance(res, dict) and "error" in res:
        raise RuntimeError(res["error"])
    if r.returncode:
        raise RuntimeError(f"AT-SPI helper exited {r.returncode}: {r.stderr[:500]}")
    return res


def snapshot(a):
    depth = a.get("max_depth") or 30
    argv = ["tree", "--max-depth", depth if 0 < depth <= 50 else 30]
    if a.get("app"):
        argv += ["--app", a["app"]]
    els = helper(*argv)["elements"]
    with open(SNAP, "w") as f:
        json.dump({e["id"]: [e["role"], e.get("name", "")] for e in els}, f)
    return els


def limit(a, default):
    n = a.get("limit") or 0
    return n if 0 < n <= 2000 else default


def matches(e, text, role):
    role = ALIASES.get((role or "").lower(), role or "")
    if role and e["role"].lower() != role.lower():
        return False
    t = (text or "").lower()
    return not t or t in e.get("name", "").lower() or t in e.get("text", "").lower()


def cu_tree(a):
    role = a.get("role") or ""
    out = [e for e in snapshot(a) if not role or e["role"].lower() == role.lower() or e["d"] <= 1]
    n = limit(a, 400)
    res = {"elements": out[:n], "count": len(out)}
    if len(out) > n:
        res["truncated"] = f"showing {n} of {len(out)}; narrow with app, role or max_depth"
    return res


def cu_find(a):
    if not a.get("text") and not a.get("role"):
        raise ValueError("give text and/or role")
    out = [e for e in snapshot(a) if e["d"] > 0 and matches(e, a.get("text"), a.get("role"))]
    return {"elements": out[:limit(a, 50)], "count": len(out)}


def cu_act(a):
    action = a.get("action")
    if action not in ("click", "focus", "set_text", "press"):
        raise ValueError(f"action {action!r} (click|focus|set_text|press)")
    try:
        with open(SNAP) as f:
            snap = json.load(f)
    except (OSError, ValueError):
        snap = {}
    eid = a.get("element_id")
    if eid not in snap:
        raise LookupError(f"unknown element_id {eid!r}: take a cu_tree or cu_find snapshot first")
    argv = ["act", eid, action, "--role", snap[eid][0], "--name", snap[eid][1]]
    if action == "set_text":
        if a.get("text") is None:
            raise ValueError("set_text needs text")
        argv += ["--text", a["text"]]
    return helper(*argv, timeout=15)


def do(a):
    """Oracle helper: the n-th element matching text/role (in app), then act on it."""
    els = cu_find({k: a[k] for k in ("text", "role", "app") if a.get(k)} | {"limit": 2000})["elements"]
    if len(els) <= a.get("index", 0):
        raise LookupError(f"no element matches {a}")
    e = els[a.get("index", 0)]
    if a.get("action") == "locate":
        return e
    return cu_act({"element_id": e["id"], "action": a.get("action", "click"), "text": a.get("value")})


TOOLS = {"cu_tree": cu_tree, "cu_find": cu_find, "cu_act": cu_act, "do": do}

if __name__ == "__main__":
    try:
        if len(sys.argv) < 2 or sys.argv[1] not in TOOLS:
            raise ValueError(f"usage: cu_atspi.py {{{'|'.join(TOOLS)}}} 'JSON'")
        args = json.loads(sys.argv[2]) if len(sys.argv) > 2 else {}
        print(json.dumps(TOOLS[sys.argv[1]](args), ensure_ascii=False))
    except Exception as e:  # one line on stderr, exit 1: the panda-cu call contract
        print(f"{type(e).__name__}: {e}", file=sys.stderr)
        sys.exit(1)
