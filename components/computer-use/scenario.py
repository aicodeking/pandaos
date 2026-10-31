"""vmtest client: one `panda-cu mcp` stdio session as the agent user, the
whole computer-use flow, asserting as it goes. Last line: RESULT {json}.
argv[1] = path of panda-cu."""
import base64
import json
import os
import random
import statistics
import subprocess
import sys
import time

HOME = os.environ["HOME"]
srv = subprocess.Popen([sys.argv[1], "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
_id = 0
metrics = {}


def log(*a):
    print(*a, file=sys.stderr, flush=True)


def rpc(method, params=None):
    global _id
    _id += 1
    srv.stdin.write(json.dumps({"jsonrpc": "2.0", "id": _id, "method": method, "params": params or {}}) + "\n")
    srv.stdin.flush()
    while True:
        line = srv.stdout.readline()
        if not line:
            raise SystemExit("panda-cu mcp exited")
        r = json.loads(line)
        if r.get("id") == _id:
            return r


def call(name, args=None, ok=True):
    t0 = time.monotonic()
    r = rpc("tools/call", {"name": name, "arguments": args or {}})["result"]
    ms = (time.monotonic() - t0) * 1000
    txt = next((c["text"] for c in r["content"] if c["type"] == "text"), "")
    log(f"{name} {json.dumps(args)[:120]} -> {'ERR ' if r['isError'] else ''}{txt[:300]} ({ms:.0f} ms)")
    if ok and r["isError"]:
        raise SystemExit(f"FAIL {name} {args}: {txt}")
    return r, ms


def data(r):
    return json.loads(next(c["text"] for c in r["content"] if c["type"] == "text"))


def image(r):
    return base64.b64decode(next(c["data"] for c in r["content"] if c["type"] == "image"))


def png_size(b):
    assert b[1:4] == b"PNG", b[:8]
    return int.from_bytes(b[16:20], "big"), int.from_bytes(b[20:24], "big")


def wait_for(pred, timeout):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        v = pred()
        if v:
            return v
        time.sleep(0.1)
    return None


def read(path):
    try:
        with open(path) as f:
            return f.read()
    except FileNotFoundError:
        return None


init = rpc("initialize", {"protocolVersion": "2025-06-18"})["result"]
assert init["serverInfo"]["name"] == "panda-cu", init
tools = [t["name"] for t in rpc("tools/list")["result"]["tools"]]
assert len(tools) == 17 and "cu_screenshot" in tools, tools

# 1. screenshot size and latency
lat = []
for _ in range(6):
    r, ms = call("cu_screenshot")
    lat.append(ms)
    assert png_size(image(r)) == (1280, 800), png_size(image(r))
metrics["screenshot_ms_median"] = round(statistics.median(lat[1:]), 1)

# 2. editor: launch, find the text area, write text
call("cu_launch", {"app": "editor"})
call("cu_wait", {"app": "mousepad", "role": "text", "timeout": 60})
els = data(call("cu_tree", {"app": "mousepad", "role": "text"})[0])["elements"]
area = next(e for e in els if e["role"] == "text" and "editable" in e.get("states", []))
call("cu_act", {"element_id": area["id"], "action": "set_text", "text": "Hello, "})
call("cu_act", {"element_id": area["id"], "action": "focus"})
call("cu_key", {"combo": "ctrl+End"})
call("cu_type", {"text": "panda-cu!"})
want = "Hello, panda-cu!"
call("cu_wait", {"app": "mousepad", "text": want, "timeout": 10})

# 3. ctrl+s, then the GTK save dialog: type the full path into Name, Enter
target = os.path.join(HOME, "hello.txt")
call("cu_key", {"combo": "ctrl+s"})
# The dialog title differs between versions ("Save As", "Save File"): the
# window wait is informative only; the Save button through AT-SPI decides.
r, _ = call("cu_wait", {"window": "Save", "timeout": 10}, ok=False)
if r["isError"]:
    call("cu_windows")
call("cu_wait", {"app": "mousepad", "role": "push button", "text": "Save", "timeout": 30})
call("cu_key", {"combo": "ctrl+a"})
call("cu_type", {"text": target})
call("cu_key", {"combo": "Return"})
saved = wait_for(lambda: read(target), 15)
metrics["save_via"] = "keyboard"
if saved is None:  # fallback: set the Name field through AT-SPI and press Save
    metrics["save_via"] = "atspi"
    name = data(call("cu_find", {"app": "mousepad", "role": "text"})[0])["elements"]
    field = next(e for e in name if "editable" in e.get("states", []) and e["id"] != area["id"])
    call("cu_act", {"element_id": field["id"], "action": "set_text", "text": target})
    btn = next(e for e in data(call("cu_find", {"app": "mousepad", "role": "push button", "text": "Save"})[0])["elements"])
    call("cu_act", {"element_id": btn["id"], "action": "click"})
    saved = wait_for(lambda: read(target), 15)
assert saved is not None, "hello.txt not saved"
metrics["saved_exact"] = saved == want
assert saved in (want, want + "\n"), repr(saved)

# 4. Set-of-Marks
r, _ = call("cu_screenshot", {"marks": True})
marks = data(r)["elements"]
assert marks and png_size(image(r)) == (1280, 800), marks
metrics["marks_editor"] = len(marks)

# 5. click accuracy on a grid of 80 GTK buttons
clicks = os.path.join(HOME, "cu-clicks.log")
call("cu_launch", {"app": "buttons"})
call("cu_wait", {"app": "cu-buttons", "role": "push button", "text": "B79", "timeout": 60})
buttons = [e for e in data(call("cu_tree", {"app": "cu-buttons", "role": "push button", "limit": 400})[0])["elements"]
           if e["role"] == "push button"]
assert len(buttons) == 80, len(buttons)
rng = random.Random(7)
hits, click_ms = {"element_id": 0, "coords": 0}, []
for i, b in enumerate(rng.sample(buttons, 20)):
    before = len((read(clicks) or "").splitlines())
    if i < 10:
        mode, args = "element_id", {"element_id": b["id"]}
    else:
        x, y, w, h = b["bbox"]
        mode, args = "coords", {"x": rng.randint(x + w // 4, x + 3 * w // 4), "y": rng.randint(y + h // 4, y + 3 * h // 4)}
    _, ms = call("cu_click", args)
    click_ms.append(ms)
    got = wait_for(lambda: (read(clicks) or "").splitlines()[before:], 2)
    log(f"  {mode} {b['name']} -> {got}")
    hits[mode] += got == [b["name"]]
assert hits["element_id"] >= 1 and hits["coords"] >= 1, hits
metrics["click_accuracy"] = (hits["element_id"] + hits["coords"]) / 20
metrics["click_hits"] = hits
metrics["click_ms_median"] = round(statistics.median(click_ms), 1)

# cu_act click (AT-SPI action, no pixels)
before = len((read(clicks) or "").splitlines())
call("cu_act", {"element_id": buttons[3]["id"], "action": "click"})
assert wait_for(lambda: (read(clicks) or "").splitlines()[before:] == [buttons[3]["name"]], 3), "cu_act click"

# 6. clipboard round trip
clip = "panda-cu clipboard ü ✓ 42"
call("cu_clipboard_set", {"text": clip})
got = data(call("cu_clipboard_get")[0])["text"]
assert got == clip, repr(got)

# 7. windows, focus, action+screenshot, scroll, drag
ws = data(call("cu_windows")[0])
assert len(ws) >= 2, ws
call("cu_focus", {"window": "mousepad"})
r, ms = call("cu_move", {"x": 10, "y": 10, "screenshot": True})
assert png_size(image(r)) == (640, 400), png_size(image(r))
metrics["move_with_screenshot_ms"] = round(ms, 1)
call("cu_scroll", {"dy": 2, "at": {"x": 640, "y": 400}})
call("cu_drag", {"from": {"x": 100, "y": 100}, "to": {"x": 300, "y": 120}})

# 8. safety: refused inputs
for name, args in [("cu_click", {"x": 1280, "y": 0}), ("cu_click", {"x": 5, "y": -1}),
                   ("cu_type", {"text": "x" * 4001}), ("cu_launch", {"app": "sh"}),
                   ("cu_key", {"combo": "ctrl+$(id)"}), ("cu_open", {"url": "file:///etc/shadow"})]:
    r, _ = call(name, args, ok=False)
    assert r["isError"], (name, args)

# 9. file manager (recorded, not asserted)
r, _ = call("cu_open", {"path": HOME}, ok=False)
metrics["files_window"] = (not r["isError"]) and data(r).get("window") is not None

print("RESULT " + json.dumps(metrics), flush=True)
srv.stdin.close()
srv.wait(timeout=10)
