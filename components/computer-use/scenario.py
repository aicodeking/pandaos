"""vmtest client: one `panda-cu mcp` stdio session as the agent user.

Each section runs even if an earlier one failed, so one CI run reports
every failure; the exit code is non-zero if any section failed. The last
line is RESULT {metrics}. argv: panda-cu path, form.html path.
"""
import base64
import http.server
import json
import os
import random
import statistics
import subprocess
import sys
import threading
import time
import traceback

HOME = os.environ["HOME"]
srv = subprocess.Popen([sys.argv[1], "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
FORM = sys.argv[2]
_id = 0
metrics, failures = {}, []


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
    log(f"{name} {json.dumps(args)[:140]} -> {'ERR ' if r['isError'] else ''}{txt[:400]} ({ms:.0f} ms)")
    if ok and r["isError"]:
        raise AssertionError(f"{name} {args}: {txt}")
    return r, ms


def text(r):
    return next(c["text"] for c in r["content"] if c["type"] == "text")


def data(r):
    return json.loads(text(r))


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


def lines(path):
    return (read(path) or "").splitlines()


def find(**args):
    return data(call("cu_find", args)[0])["elements"]


def one(**args):
    els = find(**args)
    assert els, f"no element for {args}"
    return els[0]


def diag(tag, app=None):
    """Window list, a tree and a half-size screenshot, into the log."""
    call("cu_windows", ok=False)
    r, _ = call("cu_screenshot", {"scale": 0.5}, ok=False)
    if not r["isError"]:
        log(f"DIAGPNG {tag} " + base64.b64encode(image(r)).decode())
    r, _ = call("cu_tree", {"app": app, "limit": 200} if app else {"limit": 200}, ok=False)
    log(f"TREE {tag} " + (text(r)[:12000] if not r["isError"] else "error"))


def section(name):
    def run(fn):
        log(f"=== {name}")
        try:
            fn()
        except Exception as e:  # noqa: BLE001 - every failure is reported
            log(traceback.format_exc())
            failures.append(f"{name}: {e}")
            try:
                diag(name)
            except Exception:  # noqa: BLE001
                pass
        return fn
    return run


def p50(xs):
    return round(statistics.median(xs), 1)


init = rpc("initialize", {"protocolVersion": "2025-06-18"})["result"]
assert init["serverInfo"]["name"] == "panda-cu", init
tools = [t["name"] for t in rpc("tools/list")["result"]["tools"]]
assert len(tools) == 17 and "cu_screenshot" in tools, tools


@section("screenshot")
def _():
    lat = []
    for _ in range(6):
        r, ms = call("cu_screenshot")
        lat.append(ms)
        assert png_size(image(r)) == (1280, 800), png_size(image(r))
    metrics["screenshot_ms_median"] = p50(lat[1:])


TARGET = os.path.join(HOME, "hello.txt")
WANT = "Hello, panda-cu!"


@section("editor and the save dialog, by keyboard")
def _():
    call("cu_launch", {"app": "editor"})
    call("cu_wait", {"app": "mousepad", "role": "text", "timeout": 60})
    area = next(e for e in find(app="mousepad", role="text") if "editable" in e.get("states", []))
    call("cu_act", {"element_id": area["id"], "action": "set_text", "text": "Hello, "})
    call("cu_act", {"element_id": area["id"], "action": "focus"})
    call("cu_key", {"combo": "ctrl+End"})
    call("cu_type", {"text": "panda-cu!"})
    call("cu_wait", {"app": "mousepad", "text": WANT, "timeout": 10})
    call("cu_key", {"combo": "ctrl+s"})
    call("cu_wait", {"window": "Save", "timeout": 20})
    call("cu_wait", {"app": "mousepad", "role": "push button", "text": "Save", "timeout": 20})
    # Where does the keyboard go? The Name entry must have focus.
    name = [e for e in find(app="mousepad", role="text")
            if "editable" in e.get("states", []) and e["win"] != area["win"]]
    metrics["save_name_focused"] = bool(name) and "focused" in name[0].get("states", [])
    log("NAME ENTRY", name)
    tried = []
    for how, name_text, enter in (("fullpath", TARGET, "Return"), ("filename", "hello.txt", "Return"),
                                  ("filename+KP_Enter", "hello.txt", "KP_Enter")):
        tried.append(how)
        call("cu_key", {"combo": "ctrl+a"})
        call("cu_type", {"text": name_text})
        r, _ = call("cu_wait", {"app": "mousepad", "role": "text", "text": name_text, "timeout": 5}, ok=False)
        metrics[f"save_{how}_typed"] = not r["isError"]
        tops = data(call("cu_tree", {"app": "mousepad", "max_depth": 1})[0])["elements"]
        log("TOPLEVELS before", enter, [(e["role"], e["name"], e.get("states")) for e in tops])
        call("cu_key", {"combo": enter})
        if wait_for(lambda: read(TARGET) is not None, 6):
            break
        diag(f"save-{how}", "mousepad")
    metrics["save_keys_tried"] = tried
    saved = read(TARGET)
    if saved is None:
        # Separately reported fallback; the section still fails.
        name = [e for e in find(app="mousepad", role="text")
                if "editable" in e.get("states", []) and e["win"] != area["win"]]
        btn = one(app="mousepad", role="push button", text="Save")
        if name:
            call("cu_act", {"element_id": name[0]["id"], "action": "set_text", "text": TARGET})
            call("cu_act", {"element_id": btn["id"], "action": "click"})
        metrics["save_atspi_fallback"] = wait_for(lambda: read(TARGET) is not None, 10) is not None
        raise AssertionError("the save dialog did not save by keyboard: " + ", ".join(tried))
    metrics["save_via"] = "keyboard:" + tried[-1]
    metrics["saved_exact"] = saved == WANT
    assert saved in (WANT, WANT + "\n"), repr(saved)


@section("Set-of-Marks")
def _():
    r, _ = call("cu_screenshot", {"marks": True})
    marks = data(r)["elements"]
    assert marks and png_size(image(r)) == (1280, 800), marks
    metrics["marks_editor"] = len(marks)


@section("speed of the accessibility tools")
def _():
    find_ms, act_ms, wait_ms = [], [], []
    area = next(e for e in find(app="mousepad", role="text") if "editable" in e.get("states", []))
    for _ in range(10):
        find_ms.append(call("cu_find", {"app": "mousepad", "role": "text"})[1])
        act_ms.append(call("cu_act", {"element_id": area["id"], "action": "focus"})[1])
        r, ms = call("cu_wait", {"app": "mousepad", "role": "text", "timeout": 5})
        wait_ms.append(ms / data(r)["polls"])
    metrics["find_ms_p50"], metrics["act_ms_p50"], metrics["wait_poll_ms_p50"] = p50(find_ms), p50(act_ms), p50(wait_ms)
    slow = {k: metrics[k] for k in ("find_ms_p50", "act_ms_p50", "wait_poll_ms_p50") if metrics[k] >= 200}
    assert not slow, f"p50 over 200 ms: {slow}"


CLICKS = os.path.join(HOME, "cu-clicks.log")
LAB = os.path.join(HOME, "cu-lab.log")
GRID = []


@section("click accuracy: 80-button grid")
def _():
    call("cu_launch", {"app": "buttons"})
    call("cu_wait", {"app": "cu-buttons", "role": "push button", "text": "B79", "timeout": 60})
    GRID[:] = [e for e in find(app="cu-buttons", role="push button", limit=200)]
    assert len(GRID) == 80, len(GRID)
    rng = random.Random(7)
    hits, click_ms = {"element_id": 0, "coords": 0}, []
    for i, b in enumerate(rng.sample(GRID, 20)):
        before = len(lines(CLICKS))
        if i < 10:
            mode, args = "element_id", {"element_id": b["id"]}
        else:
            x, y, w, h = b["bbox"]
            mode, args = "coords", {"x": rng.randint(x + w // 4, x + 3 * w // 4), "y": rng.randint(y + h // 4, y + 3 * h // 4)}
        _, ms = call("cu_click", args)
        click_ms.append(ms)
        got = wait_for(lambda: lines(CLICKS)[before:], 2)
        log(f"  {mode} {b['name']} -> {got}")
        hits[mode] += got == [b["name"]]
    metrics["click_accuracy"] = (hits["element_id"] + hits["coords"]) / 20
    metrics["click_hits"] = hits
    metrics["click_ms_median"] = p50(click_ms)
    assert metrics["click_accuracy"] == 1.0, hits
    before = len(lines(CLICKS))
    call("cu_act", {"element_id": GRID[3]["id"], "action": "click"})
    assert wait_for(lambda: lines(CLICKS)[before:] == [GRID[3]["name"]], 3), "cu_act click"


def lab_event(timeout=3):
    before = len(lines(LAB))

    def got():
        new = lines(LAB)[before:]
        return new if new else None
    return lambda: wait_for(got, timeout)


@section("lab: 24 px buttons")
def _():
    call("cu_launch", {"app": "lab"})
    call("cu_wait", {"app": "cu-lab", "role": "push button", "text": "Open dialog", "timeout": 60})
    small = sorted((e for e in find(app="cu-lab", role="push button", limit=200) if e["name"].startswith("s") and e["name"][1:].isdigit()),
                   key=lambda e: int(e["name"][1:]))
    assert len(small) == 12, [e["name"] for e in small]
    metrics["small_size"] = small[0]["bbox"][2:]
    rng, hits = random.Random(3), 0
    for i, b in enumerate(small):
        x, y, w, h = b["bbox"]
        args = {"element_id": b["id"]} if i % 2 == 0 else {"x": rng.randint(x + w // 4, x + 3 * w // 4), "y": rng.randint(y + h // 4, y + 3 * h // 4)}
        ev = lab_event()
        call("cu_click", args)
        got = ev()
        log("  small", b["name"], b["bbox"], "->", got)
        hits += got == [f"small:{b['name']}"]
    metrics["small_accuracy"] = hits / 12
    assert hits == 12, f"{hits}/12 small buttons hit"


@section("lab: menu item and dialog button by element_id")
def _():
    menu = one(app="cu-lab", role="menu", text="Actions")
    call("cu_click", {"element_id": menu["id"]})
    hello = json.loads(text(call("cu_wait", {"app": "cu-lab", "role": "menu item", "text": "Say hello", "timeout": 10})[0]))["match"]
    metrics["menu_item_bbox"] = hello.get("bbox")
    ev = lab_event()
    call("cu_click", {"element_id": hello["id"]})
    got = ev()
    metrics["menu_click"] = got == ["menu:hello"]
    assert got == ["menu:hello"], got
    # The menu again, then "Open dialog", then the dialog's Confirm button.
    call("cu_click", {"element_id": menu["id"]})
    item = json.loads(text(call("cu_wait", {"app": "cu-lab", "role": "menu item", "text": "Open dialog", "timeout": 10})[0]))["match"]
    call("cu_click", {"element_id": item["id"]})
    call("cu_wait", {"window": "CU Confirm", "timeout": 10})
    confirm = json.loads(text(call("cu_wait", {"app": "cu-lab", "role": "push button", "text": "Confirm", "timeout": 10})[0]))["match"]
    ev = lab_event()
    call("cu_click", {"element_id": confirm["id"]})
    got = ev()
    metrics["dialog_click"] = got == ["dialog:confirm"]
    assert got == ["dialog:confirm"], got


@section("lab: fixed-size message dialog: hit or refuse, never miss")
def _():
    call("cu_wait", {"window": "CU Confirm", "gone": True, "timeout": 10})
    call("cu_click", {"element_id": one(app="cu-lab", role="push button", text="Open message")["id"]})
    ok = json.loads(text(call("cu_wait", {"app": "cu-lab", "role": "push button", "text": "OK", "timeout": 10})[0]))["match"]
    ev = lab_event()
    r, _ = call("cu_click", {"element_id": ok["id"]}, ok=False)
    if r["isError"]:
        metrics["message_dialog"] = "refused"
        assert "position is unknown" in text(r), text(r)
        call("cu_act", {"element_id": ok["id"], "action": "click"})
        assert ev() == ["message:ok"], "cu_act on the refused button"
    else:
        got = ev()
        metrics["message_dialog"] = "hit" if got == ["message:ok"] else f"MISS {got}"
        assert got == ["message:ok"], f"click by element_id missed: {got}"


@section("lab: scroll until a row is visible, then click it")
def _():
    call("cu_wait", {"app": "cu-lab", "role": "push button", "text": "OK", "gone": True, "timeout": 10})
    assert not find(app="cu-lab", role="push button", text="row59"), "row59 is visible before scrolling"
    r0 = one(app="cu-lab", role="push button", text="row0")
    at = {"x": r0["bbox"][0] + r0["bbox"][2] // 2, "y": r0["bbox"][1] + 60}
    for notches in range(1, 16):
        call("cu_scroll", {"dy": 3, "at": at})
        rows = find(app="cu-lab", role="push button", text="row59")
        if rows and rows[0]["name"] == "row59":
            break
    else:
        raise AssertionError("row59 never became visible")
    metrics["scroll_calls"] = notches
    ev = lab_event()
    call("cu_click", {"element_id": rows[0]["id"]})
    got = ev()
    metrics["scroll_verified"] = got == ["row:59"]
    assert got == ["row:59"], got


@section("lab: drag the slider from its element_id")
def _():
    s = one(app="cu-lab", role="slider", text="Level")
    x, y, w, h = s["bbox"]
    ev = lab_event()
    call("cu_drag", {"from": {"element_id": s["id"]}, "to": {"x": x + int(w * 0.9), "y": y + h // 2}})
    got = ev()
    log("  slider", s["bbox"], "->", got)
    assert got and got[-1].startswith("scale:"), got
    metrics["drag_value"] = int(got[-1].split(":")[1])
    assert 75 <= metrics["drag_value"] <= 100, metrics["drag_value"]


@section("lab: Unicode, multi-line cu_type")
def _():
    notes = one(app="cu-lab", role="text", text="Notes")
    call("cu_act", {"element_id": notes["id"], "action": "focus"})
    typed = "Grüße ✓ ñ €\nzweite Zeile — 日本語\n\tTab"
    call("cu_type", {"text": typed})
    call("cu_wait", {"app": "cu-lab", "text": "zweite Zeile", "timeout": 5})
    got = one(app="cu-lab", role="text", text="Notes").get("text", "")
    metrics["unicode_exact"] = got == typed
    assert got == typed, repr(got)


SUBMITS = []


class Form(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/submit"):
            SUBMITS.append(self.path)
        body = open(FORM, "rb").read() if self.path.startswith("/form") else b"ok"
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


@section("browser form")
def _():
    httpd = http.server.HTTPServer(("127.0.0.1", 8765), Form)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    call("cu_open", {"url": "http://127.0.0.1:8765/form.html"})
    # The page's input (its label is "Name"), not Chromium's address bar.
    r, _ = call("cu_wait", {"app": "CU Form", "role": "entry", "text": "Name", "timeout": 90}, ok=False)
    metrics["browser_a11y"] = not r["isError"]
    if not r["isError"]:
        entry = data(r)["match"]
        r, _ = call("cu_click", {"element_id": entry["id"]}, ok=False)
        by_id = not r["isError"]
        if not by_id:  # reported, then the same through AT-SPI actions
            metrics["browser_click_refused"] = text(r)[:200]
            call("cu_act", {"element_id": entry["id"], "action": "focus"})
        call("cu_type", {"text": "panda"})
        send = one(app="CU Form", role="push button", text="Send")
        if by_id:
            call("cu_click", {"element_id": send["id"]})
        else:
            call("cu_act", {"element_id": send["id"], "action": "click"})
        metrics["browser_via"] = "a11y click by element_id" if by_id else "a11y cu_act"
    else:  # no AT-SPI tree: the input has autofocus, so the keyboard is enough
        diag("browser", "CU Form")
        call("cu_type", {"text": "panda"})
        call("cu_key", {"combo": "Return"})
        metrics["browser_via"] = "keyboard"
    assert wait_for(lambda: SUBMITS, 20), "the form was not submitted"
    assert "name=panda" in SUBMITS[0], SUBMITS


@section("an app killed mid-task gives clean errors")
def _():
    b = GRID[0] if GRID else one(app="cu-buttons", role="push button", text="B0")
    subprocess.run(["pkill", "-KILL", "-f", "cu-buttons"], check=False)
    time.sleep(1)
    r, _ = call("cu_click", {"element_id": b["id"]}, ok=False)
    assert r["isError"], "click on a dead app's element must fail"
    r2, _ = call("cu_act", {"element_id": b["id"], "action": "click"}, ok=False)
    assert r2["isError"], "act on a dead app's element must fail"
    metrics["killed_app_error"] = text(r)[:160]
    ex = data(call("cu_windows")[0]).get("recent_exits", [])
    assert any(e["app"] == "buttons" and not e["ok"] for e in ex), ex
    r, _ = call("cu_screenshot", {"scale": 0.25})
    assert png_size(image(r)) == (320, 200)


@section("an app that exits before its window is an error")
def _():
    r, _ = call("cu_launch", {"app": "crasher"}, ok=False)
    assert r["isError"] and "exit status 7" in text(r) and "crash-test" in text(r), text(r)


@section("clipboard round trip")
def _():
    clip = "panda-cu clipboard ü ✓ 42"
    _, ms = call("cu_clipboard_set", {"text": clip})
    metrics["clipboard_set_ms"] = round(ms, 1)
    got = data(call("cu_clipboard_get")[0])["text"]
    assert got == clip, repr(got)


@section("windows, focus, settled screenshot")
def _():
    ws = data(call("cu_windows")[0])["windows"]
    assert len(ws) >= 2, ws
    assert all("maximized" in w.get("states", []) for w in ws), ws
    call("cu_focus", {"window": "mousepad"})
    r, ms = call("cu_move", {"x": 10, "y": 10, "screenshot": True})
    assert png_size(image(r)) == (640, 400), png_size(image(r))
    metrics["move_with_screenshot_ms"] = round(ms, 1)
    metrics["settled"] = json.loads(r["content"][2]["text"])["settled"]


@section("refused inputs")
def _():
    for name, args in [("cu_click", {"x": 1280, "y": 0}), ("cu_click", {"x": 5, "y": -1}),
                       ("cu_type", {"text": "x" * 4001}), ("cu_launch", {"app": "sh"}),
                       ("cu_key", {"combo": "ctrl+$(id)"}), ("cu_open", {"url": "file:///etc/shadow"}),
                       ("cu_click", {"element_id": "e999999"})]:
        r, _ = call(name, args, ok=False)
        assert r["isError"], (name, args)


@section("file manager")
def _():
    r, _ = call("cu_open", {"path": HOME})
    assert data(r).get("window"), data(r)


metrics["failures"] = failures
print("RESULT " + json.dumps(metrics), flush=True)
srv.stdin.close()
srv.wait(timeout=10)
sys.exit(1 if failures else 0)
