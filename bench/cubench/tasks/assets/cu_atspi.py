#!/usr/bin/env python3
"""The AT-SPI baseline for `--tools atspi`, the same on every OS.

  cu_atspi.py cu_tree|cu_find|cu_act 'JSON-ARGS'   prints one JSON value
  cu_atspi.py do 'JSON-ARGS'                        oracle helper: find + act
  cu_atspi.py serve                                 (started on demand)

cu_tree, cu_find and cu_act take panda-cu's argument schemas and give its
results, because they map onto the same long-lived AT-SPI helper the way
components/computer-use/a11y.go does: panda-cu-atspi where installed
(PandaOS), else a copy of components/computer-use/atspi.py uploaded next to
this file, run with the system python3 + gi. The helper's element ids live
as long as its process, so the first call starts a per-session relay
(`serve`, a unix socket in $XDG_RUNTIME_DIR) that keeps one helper running;
a snapshot restore discards it with the rest of the session.
Errors: exit 1 with the message on stderr.
"""
import json
import os
import shutil
import socket
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
SOCK = os.path.join(os.environ.get("XDG_RUNTIME_DIR") or "/tmp", "cubench-atspi.sock")
ALIASES = {"button": "push button"}


def norm_role(r):
    r = (r or "").lower()
    return ALIASES.get(r, r)


# -- relay (one helper per session) -----------------------------------------------
def serve():
    exe = shutil.which("panda-cu-atspi")
    cmd = [exe] if exe else [sys.executable, os.path.join(HERE, "atspi.py")]
    helper = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
    known, seq = {}, 0
    try:
        os.unlink(SOCK)
    except FileNotFoundError:
        pass
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(SOCK)
    srv.listen(4)
    while True:
        conn, _ = srv.accept()
        with conn, conn.makefile("rw") as f:
            try:
                req = json.loads(f.readline())
                if req.get("cmd") == "act":  # role/name as seen, so a changed element is refused
                    e = known.get(req.get("id"))
                    if e is None:
                        raise LookupError(f"unknown element_id {req.get('id')!r}: take a cu_tree or cu_find snapshot first")
                    req.update(role=e[0], name=e[1])
                seq += 1
                req["seq"] = seq
                helper.stdin.write(json.dumps(req) + "\n")
                res = json.loads(helper.stdout.readline() or '{"error": "AT-SPI helper exited"}')
                for el in res.get("elements", []):
                    known[el["id"]] = (el.get("role", ""), el.get("name", ""))
                res.pop("seq", None)
            except Exception as e:  # noqa: BLE001 - answer, never hang the client
                res = {"error": f"{type(e).__name__}: {e}"}
            f.write(json.dumps(res, ensure_ascii=False) + "\n")
            f.flush()


def ask(req, timeout=30):
    s, spawned, end = None, False, time.monotonic() + 8
    while s is None:
        try:
            s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            s.settimeout(timeout)
            s.connect(SOCK)
        except OSError:
            s = None
            if time.monotonic() > end:
                raise RuntimeError("AT-SPI relay did not start") from None
            if not spawned:
                subprocess.Popen([sys.executable, os.path.abspath(__file__), "serve"], start_new_session=True,
                                 stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                spawned = True
            time.sleep(0.1)
    with s, s.makefile("rw") as f:
        f.write(json.dumps(req) + "\n")
        f.flush()
        res = json.loads(f.readline() or "{}")
    if not isinstance(res, dict) or "error" in res:
        raise RuntimeError(res.get("error") if isinstance(res, dict) else f"bad reply {res!r}")
    return res


# -- the three tools, as panda-cu maps them ------------------------------------------
def limit(a, default):
    n = a.get("limit") or 0
    return n if 0 < n <= 2000 else default


def cu_tree(a):
    depth = a.get("max_depth") or 30
    res = ask({"cmd": "tree", "app": a.get("app", ""), "max_depth": depth if 0 < depth <= 50 else 30, "limit": 2000})
    role = norm_role(a.get("role"))
    out = [e for e in res.get("elements", []) if not role or e.get("role", "").lower() == role or e.get("d", 0) <= 1]
    n = limit(a, 400)
    r = {"elements": out[:n], "count": len(out)}
    if len(out) > n:
        r["truncated"] = f"showing {n} of {len(out)}; narrow with app, role or max_depth"
    return r


def cu_find(a):
    if not a.get("text") and not a.get("role"):
        raise ValueError("give text and/or role")
    res = ask({"cmd": "find", "app": a.get("app", ""), "role": norm_role(a.get("role")), "text": a.get("text", ""),
               "limit": limit(a, 50)})
    els = res.get("elements", [])
    return {"elements": els, "count": len(els)}


def cu_act(a):
    action = a.get("action")
    if action not in ("click", "focus", "set_text", "press"):
        raise ValueError(f"action {action!r} (click|focus|set_text|press)")
    req = {"cmd": "act", "id": a.get("element_id"), "action": action}
    if action == "set_text":
        if a.get("text") is None:
            raise ValueError("set_text needs text")
        req["text"] = a["text"]
    return ask(req, timeout=20)


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
    if sys.argv[1:2] == ["serve"]:
        serve()
    try:
        if len(sys.argv) < 2 or sys.argv[1] not in TOOLS:
            raise ValueError(f"usage: cu_atspi.py {{{'|'.join(TOOLS)}|serve}} 'JSON'")
        args = json.loads(sys.argv[2]) if len(sys.argv) > 2 else {}
        print(json.dumps(TOOLS[sys.argv[1]](args), ensure_ascii=False))
    except Exception as e:  # one line on stderr, exit 1: the panda-cu call contract
        print(f"{type(e).__name__}: {e}", file=sys.stderr)
        sys.exit(1)
