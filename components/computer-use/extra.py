"""vmtest checks that need root between steps; run as `agent`.

  extra.py revoke PANDA_CU   Super+Escape mid-typing: typing stops at once
  extra.py restart PANDA_CU  one MCP session across `systemctl restart panda-cu`

Prints RESULT {json}; exits non-zero on failure.
"""
import json
import os
import subprocess
import sys
import time

CU = sys.argv[2]
HOME = os.environ["HOME"]


class MCP:
    def __init__(self, *args):
        self.p = subprocess.Popen([CU, "mcp", *args], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
        self.id = 0
        self.send("initialize", {"protocolVersion": "2025-06-18"})
        self.read()

    def send(self, method, params):
        self.id += 1
        self.p.stdin.write(json.dumps({"jsonrpc": "2.0", "id": self.id, "method": method, "params": params}) + "\n")
        self.p.stdin.flush()
        return self.id

    def read(self):
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise SystemExit("panda-cu mcp exited")
            r = json.loads(line)
            if r.get("id") == self.id:
                return r["result"]

    def call(self, name, args=None, ok=True):
        t0 = time.monotonic()
        self.send("tools/call", {"name": name, "arguments": args or {}})
        r = self.read()
        txt = r["content"][0]["text"]
        print(f"{name} {json.dumps(args)[:100]} -> {'ERR ' if r['isError'] else ''}{txt[:300]} "
              f"({(time.monotonic() - t0) * 1000:.0f} ms)", file=sys.stderr, flush=True)
        assert not ok or not r["isError"], f"{name}: {txt}"
        return r, txt


def editor_chars():
    """Characters in Mousepad's text, read through the headless session."""
    out = subprocess.run([CU, "call", "cu_find", json.dumps({"app": "mousepad", "role": "text"})],
                         capture_output=True, text=True, check=True).stdout
    els = [e for e in json.loads(out)["elements"] if "editable" in e.get("states", [])]
    return els[0].get("chars", 0)


def revoke():
    # attach mode on the agent's own (headless) session: the guard applies.
    cfg = os.path.join(HOME, "cu-attach.json")
    with open(cfg, "w") as f:
        json.dump({"mode": "attach", "env": {"XDG_RUNTIME_DIR": "/run/panda-cu", "WAYLAND_DISPLAY": "wayland-0",
                                             "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/panda-cu/bus"}}, f)
    m = MCP("-config", cfg)
    m.call("cu_focus", {"window": "mousepad"})
    area = next(e for e in json.loads(m.call("cu_find", {"app": "mousepad", "role": "text"})[1])["elements"]
                if "editable" in e.get("states", []))
    m.call("cu_act", {"element_id": area["id"], "action": "focus"})
    before = editor_chars()
    m.send("tools/call", {"name": "cu_type", "arguments": {"text": "x" * 3000}})  # ~6 s at 2 ms a key
    time.sleep(1.5)
    t0 = time.monotonic()
    subprocess.run([CU, "revoke", "-config", cfg, "10"], check=True)
    revoke_ms = (time.monotonic() - t0) * 1000
    time.sleep(0.5)
    n1 = editor_chars()
    time.sleep(2)
    n2 = editor_chars()
    typed = n1 - before
    try:
        died = m.p.wait(timeout=5) is not None
    except subprocess.TimeoutExpired:
        died = False
    r = subprocess.run([CU, "call", "-config", cfg, "cu_screenshot", "{}"], capture_output=True, text=True)
    refused = r.returncode != 0 and "stopped computer use" in r.stderr
    subprocess.run([CU, "revoke", "-config", cfg, "0"], check=True)
    res = {"revoke_ms": round(revoke_ms, 1), "typed_before_stop": typed, "typed_after_500ms": n2 - n1,
           "mcp_ended": died, "new_call_refused": refused}
    print("RESULT " + json.dumps(res), flush=True)
    assert 0 < typed < 3000, f"typing was not interrupted mid-way: {typed} of 3000"
    assert n2 == n1, f"still typing 500 ms after the stop: {n2 - n1} more characters"
    assert died and refused, res


def restart():
    m = MCP()
    m.call("cu_find", {"app": "mousepad", "role": "text"})
    open(os.path.join(HOME, "cu-step1"), "w").close()
    for _ in range(1800):  # root restarts panda-cu.service, then writes cu-step2
        if os.path.exists(os.path.join(HOME, "cu-step2")):
            break
        time.sleep(0.1)
    else:
        raise SystemExit("no restart within 180 s")
    m.call("cu_launch", {"app": "editor"})
    t0 = time.monotonic()
    r, txt = m.call("cu_find", {"app": "mousepad", "role": "text"}, ok=False)
    ms = (time.monotonic() - t0) * 1000
    print("RESULT " + json.dumps({"find_after_restart_ms": round(ms, 1), "ok": not r["isError"]}), flush=True)
    assert not r["isError"] and json.loads(txt)["count"] >= 1, txt


{"revoke": revoke, "restart": restart}[sys.argv[1]]()
